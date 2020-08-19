/*


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

package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/jackc/pgx"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cyndiv1beta1 "cyndi-operator/api/v1beta1"
)

var log = logf.Log.WithName("controller_cyndipipeline")

// CyndiPipelineReconciler reconciles a CyndiPipeline object
type CyndiPipelineReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

type ReconcileIteration struct {
	Instance *cyndiv1beta1.CyndiPipeline
	Log      logr.Logger
	AppDb    *pgx.Conn
	Client   client.Client
	Scheme   *runtime.Scheme
	Now      string
}

const cyndipipelineFinalizer = "finalizer.cyndi.cloud.redhat.com"

// +kubebuilder:rbac:groups=cyndi.cloud.redhat.com,resources=cyndipipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cyndi.cloud.redhat.com,resources=cyndipipelines/status,verbs=get;update;patch

func (r *CyndiPipelineReconciler) Reconcile(request ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)

	reqLogger.Info("Reconciling CyndiPipeline")

	instance := &cyndiv1beta1.CyndiPipeline{}

	err := r.Client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	i := ReconcileIteration{
		Instance: instance,
		Log:      reqLogger,
		Client:   r.Client,
		Scheme:   r.Scheme,
		Now:      time.Now().Format(time.RFC3339)}

	err = i.connectToAppDB()
	if err != nil {
		return reconcile.Result{}, err
	}
	defer i.closeAppDB()

	// delete pipeline
	if i.Instance.GetDeletionTimestamp() != nil {
		if contains(i.Instance.GetFinalizers(), cyndipipelineFinalizer) {
			if err := i.finalizeCyndiPipeline(); err != nil {
				return reconcile.Result{}, err
			}

			controllerutil.RemoveFinalizer(instance, cyndipipelineFinalizer)
			err := r.Client.Update(context.TODO(), instance)
			if err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	if !contains(instance.GetFinalizers(), cyndipipelineFinalizer) {
		if err := i.addFinalizer(); err != nil {
			return reconcile.Result{}, err
		}
	}

	//new pipeline
	if instance.Status.PipelineVersion == "" {
		i.refreshPipelineVersion()
		instance.Status.InitialSyncInProgress = true
	}

	dbSchema, connectorConfig, err := i.parseConfig()
	if err != nil {
		return reconcile.Result{}, err
	}

	dbTableExists, err := i.checkIfTableExists(i.Instance.Status.TableName)
	if err != nil {
		return reconcile.Result{}, err
	}

	connectorExists, err := i.checkIfConnectorExists(i.Instance.Status.ConnectorName)
	if err != nil {
		return reconcile.Result{}, err
	}

	//part, or all, of the pipeline is missing, create a new pipeline
	if dbTableExists != true || connectorExists != true {
		if instance.Status.InitialSyncInProgress != true {
			instance.Status.PreviousPipelineVersion = instance.Status.PipelineVersion
			i.refreshPipelineVersion()
		}

		err = i.createTable(i.Instance.Status.TableName, dbSchema)
		if err != nil {
			return reconcile.Result{}, err
		}

		err = i.createConnector(connectorConfig)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	pipelineIsValid, err := i.validate()
	if err != nil {
		return reconcile.Result{}, err
	}

	err = r.Client.Status().Update(context.TODO(), instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	validationFailedCountThreshold := 5
	if instance.Status.InitialSyncInProgress == true {
		validationFailedCountThreshold = 5
	}

	if pipelineIsValid != true && instance.Status.ValidationFailedCount > validationFailedCountThreshold {
		instance.Status.PreviousPipelineVersion = instance.Status.PipelineVersion
		instance.Status.ValidationFailedCount = 0
		instance.Status.PipelineVersion = ""

		err = r.Client.Status().Update(context.TODO(), instance)
		if err != nil {
			return reconcile.Result{}, err
		}
	} else if pipelineIsValid == true {
		err = i.updateView()
		if err != nil {
			return reconcile.Result{}, err
		}

		if instance.Status.PreviousPipelineVersion != "" {
			err = i.deleteTable(tableName(instance.Status.PreviousPipelineVersion))
			if err != nil {
				return reconcile.Result{}, err
			}

			err = i.deleteConnector(connectorName(instance.Status.PreviousPipelineVersion, instance.Spec.AppName))
			if err != nil {
				return reconcile.Result{}, err
			}

			instance.Status.PreviousPipelineVersion = ""
		}

		instance.Status.InitialSyncInProgress = false
	} else if pipelineIsValid != true {
		//need to sleep here. Updating the validationFailedCount in the status causes an immediate requeue of Reconcile.
		//So, setting a RequeueAfter delay will not delay the Reconcile loop.
		//https://github.com/operator-framework/operator-sdk/issues/1164#issuecomment-469485711
		//A better solution might be to create a separate controller to perform the validation. When the
		//validation_controller fails n times and needs to recreate the pipeline, it can set the status of this operator
		//to trigger a refresh.
		time.Sleep(time.Second * 15)
	}

	return i.requeue(time.Second*15, r)
}

func (i *ReconcileIteration) requeue(delay time.Duration, r *CyndiPipelineReconciler) (reconcile.Result, error) {
	err := r.Client.Status().Update(context.TODO(), i.Instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: delay, Requeue: true}, nil
}

func (r *CyndiPipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cyndiv1beta1.CyndiPipeline{}).
		Complete(r)
}

func (i *ReconcileIteration) parseConfig() (string, string, error) {
	cyndiConfig := &corev1.ConfigMap{}
	err := i.Client.Get(context.TODO(), client.ObjectKey{Name: "cyndi", Namespace: i.Instance.Namespace}, cyndiConfig)
	if err != nil {
		return "", "", err
	}
	connectorConfig := cyndiConfig.Data["connector.config"]
	dbSchema := cyndiConfig.Data["db.schema"]
	return dbSchema, connectorConfig, err
}

func (i *ReconcileIteration) refreshPipelineVersion() {
	i.Instance.Status.PipelineVersion = fmt.Sprintf(
		"1_%s",
		strconv.FormatInt(time.Now().UnixNano(), 10))
	i.Instance.Status.ConnectorName = connectorName(i.Instance.Status.PipelineVersion, i.Instance.Spec.AppName)
	i.Instance.Status.TableName = tableName(i.Instance.Status.PipelineVersion)
}

func tableName(pipelineVersion string) string {
	return fmt.Sprintf("hosts_v%s", pipelineVersion)
}

func connectorName(pipelineVersion string, appName string) string {
	return fmt.Sprintf("syndication-pipeline-%s-%s",
		appName,
		strings.Replace(pipelineVersion, "_", "-", 1))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (i *ReconcileIteration) finalizeCyndiPipeline() error {
	err := i.deleteTable(i.Instance.Status.TableName)
	if err != nil {
		return err
	}
	i.Log.Info("Successfully finalized CyndiPipeline")
	return nil
}

func (i *ReconcileIteration) addFinalizer() error {
	i.Log.Info("Adding Finalizer for the CyndiPipeline")
	controllerutil.AddFinalizer(i.Instance, cyndipipelineFinalizer)

	err := i.Client.Update(context.TODO(), i.Instance)
	if err != nil {
		i.Log.Error(err, "Failed to update CyndiPipeline with finalizer")
		return err
	}
	return nil
}
