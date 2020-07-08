package syndesis

import (
	"context"
	"reflect"
	"time"

	consolev1 "github.com/openshift/api/console/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	syndesisv1beta1 "github.com/syndesisio/syndesis/install/operator/pkg/apis/syndesis/v1beta1"
	"github.com/syndesisio/syndesis/install/operator/pkg/syndesis/action"
	"github.com/syndesisio/syndesis/install/operator/pkg/syndesis/capabilities"
	"github.com/syndesisio/syndesis/install/operator/pkg/syndesis/clienttools"
)

var log = logf.Log.WithName("controller")

var (
	actions []action.SyndesisOperatorAction
)

// Add creates a new Syndesis Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	reconciler, err := newReconciler(mgr)
	if err != nil {
		return err
	}

	return add(mgr, reconciler)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (*ReconcileSyndesis, error) {

	clientTools := &clienttools.ClientTools{}
	clientTools.SetRuntimeClient(mgr.GetClient())

	return &ReconcileSyndesis{
		clientTools: clientTools,
		scheme:      mgr.GetScheme(),
	}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r *ReconcileSyndesis) error {
	// Create a new controller
	c, err := controller.New("syndesis-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Syndesis
	err = c.Watch(&source.Kind{Type: &syndesisv1beta1.Syndesis{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	actions = action.NewOperatorActions(mgr, r.clientTools)
	return nil
}

var _ reconcile.Reconciler = &ReconcileSyndesis{}

// ReconcileSyndesis reconciles a Syndesis object
type ReconcileSyndesis struct {
	// This client kit contains a split client, initialized using mgr.Client() above,
	// that reads objects from the cache and writes to the apiserver
	clientTools *clienttools.ClientTools
	scheme      *runtime.Scheme
}

// Reconcile the state of the Syndesis infrastructure elements
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileSyndesis) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.V(2).Info("Reconciling Syndesis")

	// Fetch the Syndesis syndesis
	syndesis := &syndesisv1beta1.Syndesis{}

	ctx := context.TODO()

	client, _ := r.clientTools.RuntimeClient()
	err := client.Get(ctx, request.NamespacedName, syndesis)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			syndesis.ObjectMeta = metav1.ObjectMeta{
				Name:      request.Name,
				Namespace: request.Namespace,
			}

			//Handle removal of cluster-scope object.
			r.removeConsoleLink(ctx, syndesis)

			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.

		log.Error(err, "Cannot read object", request.NamespacedName)
		return reconcile.Result{
			Requeue:      true,
			RequeueAfter: 10 * time.Second,
		}, err
	}

	for _, a := range actions {
		// Don't want to do anything if the syndesis resource has been updated in the meantime
		// This happens when a processing takes more tha the resync period
		if latest, err := r.isLatestVersion(ctx, syndesis); err != nil || !latest {
			log.Info("syndesis resource changed in the meantime, requeue and rerun in 5 seconds", "name", syndesis.Name)
			return reconcile.Result{
				Requeue:      true,
				RequeueAfter: 5 * time.Second,
			}, nil
		}

		if a.CanExecute(syndesis) {
			log.V(2).Info("Running action", "action", reflect.TypeOf(a))
			if err := a.Execute(ctx, syndesis); err != nil {
				log.Error(err, "Error reconciling", "action", reflect.TypeOf(a), "phase", syndesis.Status.Phase)
				return reconcile.Result{
					Requeue:      true,
					RequeueAfter: 10 * time.Second,
				}, nil
			}
		}
	}

	// Requeuing because actions expect this behaviour
	return reconcile.Result{
		Requeue:      true,
		RequeueAfter: 15 * time.Second,
	}, nil
}

func (r *ReconcileSyndesis) isLatestVersion(ctx context.Context, syndesis *syndesisv1beta1.Syndesis) (bool, error) {
	refreshed := syndesis.DeepCopy()
	client, _ := r.clientTools.RuntimeClient()
	if err := client.Get(ctx, types.NamespacedName{Name: refreshed.Name, Namespace: refreshed.Namespace}, refreshed); err != nil {
		return false, err
	}
	return refreshed.ResourceVersion == syndesis.ResourceVersion, nil
}

func (r *ReconcileSyndesis) removeConsoleLink(ctx context.Context, syndesis *syndesisv1beta1.Syndesis) (request reconcile.Result, err error) {
	// Need to determine if platform is applicable first
	ac, err := capabilities.ApiCapabilities(r.clientTools)
	if err != nil {
		return reconcile.Result{}, err
	}

	if !ac.ConsoleLink {
		//
		// Nothing to do.
		// This cluster does not support the ConsoleLink API
		//
		return reconcile.Result{}, nil
	}

	consoleLinkName := syndesis.Name + "-" + syndesis.Namespace
	consoleLink := &consolev1.ConsoleLink{}
	client, _ := r.clientTools.RuntimeClient()
	err = client.Get(context.TODO(), types.NamespacedName{Name: consoleLinkName}, consoleLink)
	if err != nil {
		if !errors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
	} else {
		err = client.Delete(context.TODO(), consoleLink)
		if err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, err
}
