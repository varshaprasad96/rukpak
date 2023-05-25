package install

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/go-logr/logr"
	helmclient "github.com/operator-framework/helm-operator-plugins/pkg/client"
	rukpakv1alpha1 "github.com/operator-framework/rukpak/api/v1alpha1"
	"github.com/operator-framework/rukpak/internal/provisioner/bundle"
	helm "github.com/operator-framework/rukpak/internal/provisioner/helm"
	"github.com/operator-framework/rukpak/internal/source"
	storage "github.com/operator-framework/rukpak/internal/storage"
	util "github.com/operator-framework/rukpak/internal/util"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/postrender"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	apimachyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

const (
	defaultProvisionerClassName       = "core-rukpak-io-plain"
	defaultBundleProvisionerClassName = "core-rukpak-io-registry"
	defaultBundleCacheDir             = "/var/cache/bundles"
	errInstalling                     = "Failed creating desired bundle"
)

type InstallOptions struct {
	cfg                        *rest.Config
	Name                       string
	ProvisionerClassName       string
	BundleProvisionerClassName string
	ImageRef                   string
	httpBindAddr               string
	httpExternalAddr           string
	bundleCAFile               string
	systemNamespace            string
	namespace                  string
	unpackImage                string
	baseUploadManagerURL       string
	storageDirectory           string
	client                     client.Client
	context                    context.Context
	storage                    storage.Storage
	handler                    bundle.Handler
	acg                        helmclient.ActionClientGetter
	systemNsCluster            cluster.Cluster
}

func (i *InstallOptions) Complete() error {
	var err error

	if i.cfg == nil {
		i.cfg = ctrl.GetConfigOrDie()
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(rukpakv1alpha1.AddToScheme(scheme))

	if i.client == nil {
		i.client, err = client.New(i.cfg, client.Options{Scheme: scheme})
		if err != nil {
			return fmt.Errorf("error creating client %v", err)
		}
	}

	if i.httpBindAddr == "" {
		i.httpBindAddr = ":8080"
	}

	if i.httpExternalAddr == "" {
		i.httpExternalAddr = "http://localhost:8080"
	}

	// if i.bundleCAFile == "" {
	// 	i.bundleCAFile = "/etc/pki/tls/ca.crt"
	// }

	if i.systemNamespace == "" {
		i.systemNamespace = "rukpak-system"
	}

	i.namespace = util.PodNamespace(i.systemNamespace)
	fmt.Println("namespace: ", i.namespace, "systemNamespace: ", i.systemNamespace)
	i.systemNsCluster, err = cluster.New(i.cfg, func(opts *cluster.Options) {
		opts.Scheme = scheme
		opts.Namespace = i.systemNamespace
		opts.ClientDisableCacheFor = []client.Object{&corev1.Pod{}}
	})

	if i.unpackImage == "" {
		i.unpackImage = "quay.io/operator-framework/rukpak:main"
	}

	i.baseUploadManagerURL = fmt.Sprintf("https://%s.%s.svc", "core", i.namespace)

	if i.storageDirectory == "" {
		i.storageDirectory = defaultBundleCacheDir
	}

	storageURL, err := url.Parse(fmt.Sprintf("%s/bundles/", i.httpExternalAddr))
	if err != nil {
		return fmt.Errorf("error parsing storage URL %v", err)
	}

	localStorage := &storage.LocalDirectory{
		RootDirectory: i.storageDirectory,
		URL:           *storageURL,
	}

	var rootCAs *x509.CertPool

	if i.bundleCAFile != "" {
		var err error
		if rootCAs, err = util.LoadCertPool(i.bundleCAFile); err != nil {
			return fmt.Errorf("error loading the rootCAs %v", err)
		}
	}

	httpLoader := storage.NewHTTP(
		storage.WithRootCAs(rootCAs),
		storage.WithBearerToken(i.cfg.BearerToken),
	)

	bundleStorage := storage.WithFallbackLoader(localStorage, httpLoader)
	i.storage = bundleStorage

	i.handler = bundle.HandlerFunc(helm.HandleBundle)

	cfgGetter := helmclient.NewActionConfigGetter(i.cfg, i.client.RESTMapper(), logr.Logger{})
	i.acg = helmclient.NewActionClientGetter(cfgGetter)

	return nil
}

func (i *InstallOptions) Validate() (err error) {
	if i.Name == "" {
		return fmt.Errorf("name of the bundle deployment should be provided")
	}

	if i.ProvisionerClassName == "" {
		fmt.Println("using the default provisioner class")
		i.ProvisionerClassName = defaultProvisionerClassName
	}

	if i.BundleProvisionerClassName == "" {
		fmt.Println("using the default bundle provisioner class")
		i.BundleProvisionerClassName = defaultBundleProvisionerClassName
	}

	if i.ImageRef == "" {
		return fmt.Errorf("empty image ref not allowed")

	}

	return nil
}

func (i *InstallOptions) Run() error {
	i.context = context.TODO()
	if err := i.createBundleDeployment(); err != nil {
		return fmt.Errorf("error creating bundle deployment %q", err)
	}

	if err := i.fetchDesiredBundle(); err != nil {
		return fmt.Errorf("error running and doing other things %v", err)
	}

	return nil
}

// CreateBundleDeployment creates a bundleDeployment defined in the
// specific yaml.
func (i *InstallOptions) createBundleDeployment() error {
	fmt.Println("creating bundle deployment")
	bd := rukpakv1alpha1.BundleDeployment{
		ObjectMeta: v1.ObjectMeta{
			Name: i.Name,
		},
		Spec: rukpakv1alpha1.BundleDeploymentSpec{
			ProvisionerClassName: i.ProvisionerClassName,
			Template: &rukpakv1alpha1.BundleTemplate{
				Spec: rukpakv1alpha1.BundleSpec{
					ProvisionerClassName: i.BundleProvisionerClassName,
					Source: rukpakv1alpha1.BundleSource{
						Type: rukpakv1alpha1.SourceTypeImage,
						Image: &rukpakv1alpha1.ImageSource{
							Ref: i.ImageRef,
						},
					},
				},
			},
		},
	}

	if err := i.client.Create(i.context, &bd); err != nil {
		return err
	}
	return nil
}

func (i *InstallOptions) fetchDesiredBundle() error {
	// fetch the bundle deployment existing on cluster.
	// if not present it should error, since it should have been
	// created before.
	fetchedBundle := &rukpakv1alpha1.BundleDeployment{}
	if err := i.client.Get(i.context, types.NamespacedName{Name: i.Name}, fetchedBundle); err != nil {
		return err
	}

	bd := fetchedBundle.DeepCopy()

	bd.SetGroupVersionKind(rukpakv1alpha1.BundleDeploymentGVK)

	fmt.Println("Getting GVK here", bd.GroupVersionKind())
	bundle, allBundles, err := util.ReconcileDesiredBundle(i.context, i.client, bd)
	if err != nil {
		meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
			Type:    rukpakv1alpha1.TypeHasValidBundle,
			Status:  v1.ConditionUnknown,
			Reason:  rukpakv1alpha1.ReasonReconcileFailed,
			Message: err.Error(),
		})
		return fmt.Errorf("Failed creating desired bundle %v", err)
	}

	if bundle.Status.Phase != rukpakv1alpha1.PhaseUnpacked {
		reason := rukpakv1alpha1.ReasonUnpackPending
		status := v1.ConditionTrue
		message := fmt.Sprintf("Waiting for the %s Bundle to be unpacked", bundle.GetName())
		if bundle.Status.Phase == rukpakv1alpha1.PhaseFailing {
			reason = rukpakv1alpha1.ReasonUnpackFailed
			status = v1.ConditionFalse
			message = fmt.Sprintf("Failed to unpack the %s Bundle", bundle.GetName())
			if c := meta.FindStatusCondition(bundle.Status.Conditions, rukpakv1alpha1.TypeUnpacked); c != nil {
				message = fmt.Sprintf("%s: %s", message, c.Message)
			}
		}
		meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
			Type:    rukpakv1alpha1.TypeHasValidBundle,
			Status:  status,
			Reason:  reason,
			Message: message,
		})
	}

	meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
		Type:    rukpakv1alpha1.TypeHasValidBundle,
		Status:  v1.ConditionTrue,
		Reason:  rukpakv1alpha1.ReasonUnpackSuccessful,
		Message: fmt.Sprintf("Successfully unpacked the %s Bundle", bundle.GetName()),
	})

	// print on concole
	fmt.Println("Successfully unpacked the bundle")

	unpacker, err := source.NewDefaultUnpackerImage(i.systemNsCluster, i.namespace, i.unpackImage, i.baseUploadManagerURL, nil, i.client)
	if err != nil {
		return fmt.Errorf("error creating unpacker %v", err)
	}

	bundle.SetGroupVersionKind(rukpakv1alpha1.BundleGVK)
	unpackResult, err := unpacker.Unpack(i.context, bundle)
	if err != nil {
		return fmt.Errorf("error unpacking the bundle %v", err)
	}

	fmt.Println("bundle unpacker", unpackResult)

	storeFS, err := i.handler.Handle(i.context, unpackResult.Bundle, bundle)
	if err != nil {
		return fmt.Errorf("error handling bundle from bundle handler %q", err)
	}

	fmt.Println("****", storeFS)
	// bundleFS, err := i.storage.Load(i.context, bundle)

	// if err != nil {
	// 	return fmt.Errorf("error loading bundle %v", err)
	// }
	// fmt.Println("bundleFS", bundleFS)
	// // chrt, values, err := i.handler.Handle(i.context, bundleFS, bd)
	// bd.SetNamespace(i.systemNamespace)
	// cl, err := i.acg.ActionClientFor(bd)
	// bd.SetNamespace("")

	// post := &postrenderer{
	// 	labels: map[string]string{
	// 		util.CoreOwnerKindKey: rukpakv1alpha1.BundleDeploymentKind,
	// 		util.CoreOwnerNameKey: bd.GetName(),
	// 	},
	// }

	// rel, state, err := i.getReleaseState(cl, bd, chrt, values, post)

	// switch state {
	// case stateNeedsInstall:
	// 	rel, err = cl.Install(bd.Name, i.systemNamespace, chrt, values, func(install *action.Install) error {
	// 		install.CreateNamespace = false
	// 		return nil
	// 	},
	// 		// To be refactored issue https://github.com/operator-framework/rukpak/issues/534
	// 		func(install *action.Install) error {
	// 			post.cascade = install.PostRenderer
	// 			install.PostRenderer = post
	// 			return nil
	// 		})
	// 	if err != nil {
	// 		if isResourceNotFoundErr(err) {
	// 			err = errRequiredResourceNotFound{err}
	// 		}
	// 		meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
	// 			Type:    rukpakv1alpha1.TypeInstalled,
	// 			Status:  v1.ConditionFalse,
	// 			Reason:  rukpakv1alpha1.ReasonInstallFailed,
	// 			Message: err.Error(),
	// 		})
	// 		return err
	// 	}
	// case stateNeedsUpgrade:
	// 	rel, err = cl.Upgrade(bd.Name, i.systemNamespace, chrt, values,
	// 		// To be refactored issue https://github.com/operator-framework/rukpak/issues/534
	// 		func(upgrade *action.Upgrade) error {
	// 			post.cascade = upgrade.PostRenderer
	// 			upgrade.PostRenderer = post
	// 			return nil
	// 		})
	// 	if err != nil {
	// 		if isResourceNotFoundErr(err) {
	// 			err = errRequiredResourceNotFound{err}
	// 		}
	// 		meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
	// 			Type:    rukpakv1alpha1.TypeInstalled,
	// 			Status:  v1.ConditionFalse,
	// 			Reason:  rukpakv1alpha1.ReasonUpgradeFailed,
	// 			Message: err.Error(),
	// 		})
	// 		return err
	// 	}
	// case stateUnchanged:
	// 	if err := cl.Reconcile(rel); err != nil {
	// 		if isResourceNotFoundErr(err) {
	// 			err = errRequiredResourceNotFound{err}
	// 		}
	// 		meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
	// 			Type:    rukpakv1alpha1.TypeInstalled,
	// 			Status:  v1.ConditionFalse,
	// 			Reason:  rukpakv1alpha1.ReasonReconcileFailed,
	// 			Message: err.Error(),
	// 		})
	// 		return err
	// 	}
	// default:
	// 	return fmt.Errorf("unexpected release state %q", state)
	// }

	// relObjects, err := util.ManifestObjects(strings.NewReader(rel.Manifest), fmt.Sprintf("%s-release-manifest", rel.Name))
	// if err != nil {
	// 	meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
	// 		Type:    rukpakv1alpha1.TypeInstalled,
	// 		Status:  v1.ConditionFalse,
	// 		Reason:  rukpakv1alpha1.ReasonCreateDynamicWatchFailed,
	// 		Message: err.Error(),
	// 	})
	// 	return err
	// }

	// for _, obj := range relObjects {
	// 	_, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	// 	if err != nil {
	// 		meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
	// 			Type:    rukpakv1alpha1.TypeInstalled,
	// 			Status:  v1.ConditionFalse,
	// 			Reason:  rukpakv1alpha1.ReasonCreateDynamicWatchFailed,
	// 			Message: err.Error(),
	// 		})
	// 		return err
	// 	}
	// }
	// meta.SetStatusCondition(&bd.Status.Conditions, v1.Condition{
	// 	Type:    rukpakv1alpha1.TypeInstalled,
	// 	Status:  v1.ConditionTrue,
	// 	Reason:  rukpakv1alpha1.ReasonInstallationSucceeded,
	// 	Message: fmt.Sprintf("Instantiated bundle %s successfully", bundle.GetName()),
	// })
	// bd.Status.ActiveBundle = bundle.GetName()

	if err := i.reconcileOldBundles(i.context, bundle, allBundles); err != nil {
		return fmt.Errorf("failed to delete old bundles: %v", err)
	}

	return nil
}

// reconcileOldBundles is responsible for garbage collecting any Bundles
// that no longer match the desired Bundle template.
func (p *InstallOptions) reconcileOldBundles(ctx context.Context, currBundle *rukpakv1alpha1.Bundle, allBundles *rukpakv1alpha1.BundleList) error {
	var (
		errors []error
	)
	for i := range allBundles.Items {
		if allBundles.Items[i].GetName() == currBundle.GetName() {
			continue
		}
		if err := p.client.Delete(ctx, &allBundles.Items[i]); err != nil {
			errors = append(errors, err)
			continue
		}
	}
	return utilerrors.NewAggregate(errors)
}

type postrenderer struct {
	labels  map[string]string
	cascade postrender.PostRenderer
}

func (p *postrenderer) Run(renderedManifests *bytes.Buffer) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	dec := apimachyaml.NewYAMLOrJSONDecoder(renderedManifests, 1024)
	for {
		obj := unstructured.Unstructured{}
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		obj.SetLabels(p.labels)
		b, err := obj.MarshalJSON()
		if err != nil {
			return nil, err
		}
		buf.Write(b)
	}
	if p.cascade != nil {
		return p.cascade.Run(&buf)
	}
	return &buf, nil
}

type releaseState string

const (
	stateNeedsInstall releaseState = "NeedsInstall"
	stateNeedsUpgrade releaseState = "NeedsUpgrade"
	stateUnchanged    releaseState = "Unchanged"
	stateError        releaseState = "Error"
)

func (p *InstallOptions) getReleaseState(cl helmclient.ActionInterface, obj v1.Object, chrt *chart.Chart, values chartutil.Values, post *postrenderer) (*release.Release, releaseState, error) {
	currentRelease, err := cl.Get(obj.GetName())
	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, stateError, err
	}
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, stateNeedsInstall, nil
	}
	desiredRelease, err := cl.Upgrade(obj.GetName(), p.systemNamespace, chrt, values, func(upgrade *action.Upgrade) error {
		upgrade.DryRun = true
		return nil
	},
		// To be refactored issue https://github.com/operator-framework/rukpak/issues/534
		func(upgrade *action.Upgrade) error {
			post.cascade = upgrade.PostRenderer
			upgrade.PostRenderer = post
			return nil
		})
	if err != nil {
		return currentRelease, stateError, err
	}
	if desiredRelease.Manifest != currentRelease.Manifest ||
		currentRelease.Info.Status == release.StatusFailed ||
		currentRelease.Info.Status == release.StatusSuperseded {
		return currentRelease, stateNeedsUpgrade, nil
	}
	return currentRelease, stateUnchanged, nil
}

func isResourceNotFoundErr(err error) bool {
	var agg utilerrors.Aggregate
	if errors.As(err, &agg) {
		for _, err := range agg.Errors() {
			return isResourceNotFoundErr(err)
		}
	}

	nkme := &meta.NoKindMatchError{}
	if errors.As(err, &nkme) {
		return true
	}
	if apierrors.IsNotFound(err) {
		return true
	}

	// TODO: improve NoKindMatchError matching
	//   An error that is bubbled up from the k8s.io/cli-runtime library
	//   does not wrap meta.NoKindMatchError, so we need to fallback to
	//   the use of string comparisons for now.
	return strings.Contains(err.Error(), "no matches for kind")
}

type errRequiredResourceNotFound struct {
	error
}

func (err errRequiredResourceNotFound) Error() string {
	return fmt.Sprintf("required resource not found: %v", err.error)
}
