/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bundledeployment

import (
	"context"
	"fmt"

	"github.com/operator-framework/rukpak/api/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha2source "github.com/operator-framework/rukpak/internal/controllers/v1alpha2/source"
	"github.com/spf13/afero"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	apimacherrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// BundleDeploymentReconciler reconciles a BundleDeployment object
type bundleDeploymentReconciler struct {
	unpacker v1alpha2source.Unpacker
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	controller crcontroller.Controller
}

type Option func(bd *bundleDeploymentReconciler)

func WithUnpacker(u v1alpha2source.Unpacker) Option {
	return func(bd *bundleDeploymentReconciler) {
		bd.unpacker = u
	}
}

//+kubebuilder:rbac:groups=core.rukpak.io,resources=bundledeployments,verbs=list;watch
//+kubebuilder:rbac:groups=core.rukpak.io,resources=bundledeployments/status,verbs=update;patch
//+kubebuilder:rbac:groups=core.rukpak.io,resources=bundledeployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=*,resources=*,verbs=*

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.9.2/pkg/reconcile
func (b *bundleDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	existingBD := &v1alpha2.BundleDeployment{}
	err := b.Get(ctx, req.NamespacedName, existingBD)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("bundledeployment resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get bundledeployment")
		return ctrl.Result{}, err
	}

	reconciledBD := existingBD.DeepCopy()
	res, reconcileErr := b.reconcile(ctx, reconciledBD)
	// Update the status subresource before updating the main object. This is
	// necessary because, in many cases, the main object update will remove the
	// finalizer, which will cause the core Kubernetes deletion logic to
	// complete. Therefore, we need to make the status update prior to the main
	// object update to ensure that the status update can be processed before
	// a potential deletion.
	if !equality.Semantic.DeepEqual(existingBD.Status, reconciledBD.Status) {
		if updateErr := b.Status().Update(ctx, reconciledBD); updateErr != nil {
			return res, apimacherrors.NewAggregate([]error{reconcileErr, updateErr})
		}
	}
	existingBD.Status, reconciledBD.Status = v1alpha2.BundleDeploymentStatus{}, v1alpha2.BundleDeploymentStatus{}
	if !equality.Semantic.DeepEqual(existingBD, reconciledBD) {
		if updateErr := b.Update(ctx, reconciledBD); updateErr != nil {
			return res, apimacherrors.NewAggregate([]error{reconcileErr, updateErr})
		}
	}
	return res, reconcileErr
}

func (b *bundleDeploymentReconciler) reconcile(ctx context.Context, bd *v1alpha2.BundleDeployment) (ctrl.Result, error) {

	_, err := b.unpackContents(ctx, bd)
	if err != nil {
		setUnpackStatusFailing(&bd.Status.Conditions, fmt.Sprintf("unpacking failed %q", bd.GetName()), bd.Generation)
	}
	setUnpackStatusSuccess(&bd.Status.Conditions, fmt.Sprintf("unpacking successful %q", bd.GetName()), bd.Generation)

	return ctrl.Result{}, nil
}

// unpackContents unpacks contents from all the sources, and stores under a directory referenced by the bundle deployment name.
func (b *bundleDeploymentReconciler) unpackContents(ctx context.Context, bd *v1alpha2.BundleDeployment) (*afero.Fs, error) {
	// add status to mention that the contents are being unpacked.
	setUnpackStatusPending(&bd.Status.Conditions, fmt.Sprintf("unpacking bundledeployment %q", bd.GetName()), bd.Generation)

	// set a base filesystem path and unpack contents under the root filepath defined by
	// bundledeployment name.
	bundleDepFs := afero.NewBasePathFs(afero.NewOsFs(), bd.GetName())
	// unpackedResults := make([]v1alpha2source.Result, len(bd.Spec.Sources))
	errs := make([]error, 0)

	for _, source := range bd.Spec.Sources {
		_, err := b.unpacker.Unpack(ctx, bd.Name, &source, bundleDepFs)
		if err != nil {
			errs = append(errs, fmt.Errorf("error unpacking from %s source: %v", source.Kind, err))
		}
	}
	return &bundleDepFs, apimacherrors.NewAggregate(errs)
}

// validateContents validates if the unpacked bundle contents are of the right format.
func (b *bundleDeploymentReconciler) validateContents(bd *v1alpha2.BundleDeployment, fs *afero.Fs) error {
	format := bd.Spec.Format

	// Fetch contents from the unpacked path and pass it on for validation.
	ok, err := afero.DirExists(*fs, bd.GetName())
	if err != nil {
		return fmt.Errorf("error accessing the downloaded content %v", err)
	}

	if !ok {
		return fmt.Errorf("unpacked directory does not exist %s", bd.GetName())
	}

	if format == v1alpha2.FormatPlain {
		// Validate contents for each format
	}
	return nil
}

func SetupWithManager(mgr manager.Manager, opts ...Option) error {
	bd := &bundleDeploymentReconciler{
		Client: mgr.GetClient(),
	}
	for _, o := range opts {
		o(bd)
	}

	controller, err := ctrl.NewControllerManagedBy(mgr).For(&v1alpha2.BundleDeployment{}).Owns(&v1alpha2.BundleDeployment{}).Build(bd)
	if err != nil {
		return err
	}

	bd.controller = controller
	return nil
}

// setUnpackStatusPending sets the resolved status condition to success.
func setUnpackStatusPending(conditions *[]metav1.Condition, message string, generation int64) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               v1alpha2.TypeUnpacked,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha2.ReasonUnpacking,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// setUnpackStatusFailing sets the resolved status condition to success.
func setUnpackStatusFailing(conditions *[]metav1.Condition, message string, generation int64) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               v1alpha2.TypeUnpacked,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha2.ReasonUnpackFailed,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// setUnpackStatusSuccess sets the resolved status condition to success.
func setUnpackStatusSuccess(conditions *[]metav1.Condition, message string, generation int64) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               v1alpha2.TypeUnpacked,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha2.ReasonUnpackSuccessful,
		Message:            message,
		ObservedGeneration: generation,
	})
}
