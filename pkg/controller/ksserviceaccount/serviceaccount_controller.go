/*
 * Copyright 2024 the KubeSphere Authors.
 * Please refer to the LICENSE file in the root directory of the project.
 * https://github.com/kubesphere/kubesphere/blob/master/LICENSE
 */

package ksserviceaccount

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "kubesphere.io/api/core/v1alpha1"

	kscontroller "kubesphere.io/kubesphere/pkg/controller"
)

const (
	controllerName                  = "ks-serviceaccount"
	finalizer                       = "finalizers.kubesphere.io/serviceaccount"
	messageCreateSecretSuccessfully = "Create token secret successfully"
	reasonInvalidSecret             = "InvalidSecret"
)

var _ kscontroller.Controller = &Reconciler{}

func (r *Reconciler) Name() string {
	return controllerName
}

type Reconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Logger        logr.Logger
	EventRecorder record.EventRecorder
}

func (r *Reconciler) SetupWithManager(mgr *kscontroller.Manager) error {
	r.Client = mgr.GetClient()
	r.EventRecorder = mgr.GetEventRecorderFor(controllerName)
	r.Logger = ctrl.Log.WithName("controllers").WithName(controllerName)
	return builder.
		ControllerManagedBy(mgr).
		For(
			&corev1alpha1.ServiceAccount{},
			builder.WithPredicates(
				predicate.ResourceVersionChangedPredicate{},
			),
		).
		Named(controllerName).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 2,
		}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (reconcile.Result, error) {
	logger := r.Logger.WithValues(req.NamespacedName, "ServiceAccount")
	sa := &corev1alpha1.ServiceAccount{}
	if err := r.Get(ctx, req.NamespacedName, sa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if sa.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(sa, finalizer) {
			deepCopy := sa.DeepCopy()
			deepCopy.Finalizers = append(deepCopy.Finalizers, finalizer)
			if len(sa.Secrets) == 0 {
				secretCreated, err := r.createTokenSecret(ctx, sa)
				if err != nil {
					logger.Error(err, "create secret failed")
					return ctrl.Result{}, err
				}
				logger.V(4).WithName(secretCreated.Name).Info("secret created successfully")

				deepCopy.Secrets = append(deepCopy.Secrets, v1.ObjectReference{
					Namespace: secretCreated.Namespace,
					Name:      secretCreated.Name,
				})
				r.EventRecorder.Event(deepCopy, corev1.EventTypeNormal, kscontroller.Synced, messageCreateSecretSuccessfully)
			}
			if err := r.Update(ctx, deepCopy); err != nil {
				logger.Error(err, "update serviceaccount failed")
				return ctrl.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(sa, finalizer) {
			if err := r.deleteSecretToken(ctx, sa, logger); err != nil {
				logger.Error(err, "delete secret failed")
				return ctrl.Result{}, err
			}
			_ = controllerutil.RemoveFinalizer(sa, finalizer)
			if err := r.Update(ctx, sa); err != nil {
				logger.Error(err, "update serviceaccount failed")
				return ctrl.Result{}, err
			}
		}
	}

	if err := r.checkAllSecret(ctx, sa); err != nil {
		logger.Error(err, "failed check secrets")
		return ctrl.Result{}, err
	}

	if err := r.checkServiceAccountRefPod(ctx, sa); err != nil {
		logger.Error(err, "failed check service account ref pod")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) createTokenSecret(ctx context.Context, sa *corev1alpha1.ServiceAccount) (*v1.Secret, error) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", sa.Name),
			Namespace:    sa.Namespace,
			Annotations:  map[string]string{corev1alpha1.ServiceAccountName: sa.Name},
		},
		Type: corev1alpha1.SecretTypeServiceAccountToken,
	}

	return secret, r.Client.Create(ctx, secret)
}

func (r *Reconciler) deleteSecretToken(ctx context.Context, sa *corev1alpha1.ServiceAccount, logger logr.Logger) error {
	for _, secretName := range sa.Secrets {
		secret := &v1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: secretName.Namespace, Name: secretName.Name}, secret); err != nil {
			if errors.IsNotFound(err) {
				continue
			} else {
				return err
			}
		}
		if err := r.checkSecretToken(secret, sa.Name); err == nil {
			if err = r.Delete(ctx, secret); err != nil {
				return err
			}
			logger.V(2).WithName(secretName.Name).Info("delete secret successfully")
		}
	}
	return nil
}

func (r *Reconciler) checkAllSecret(ctx context.Context, sa *corev1alpha1.ServiceAccount) error {
	for _, secretRef := range sa.Secrets {
		secret := &v1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: sa.Namespace, Name: secretRef.Name}, secret); err != nil {
			if errors.IsNotFound(err) {
				r.EventRecorder.Event(sa, corev1.EventTypeWarning, reasonInvalidSecret, err.Error())
				continue
			}
			return err
		}
		if err := r.checkSecretToken(secret, sa.Name); err != nil {
			r.EventRecorder.Event(sa, corev1.EventTypeWarning, reasonInvalidSecret, err.Error())
		}
	}
	return nil
}

// checkSecretTokens Check if there has valid token, and the invalid token reference will be deleted
func (r *Reconciler) checkSecretToken(secret *v1.Secret, subjectName string) error {
	if secret.Type != corev1alpha1.SecretTypeServiceAccountToken {
		return fmt.Errorf("unsupported secret %s type: %s", secret.Name, secret.Type)
	}
	if saName := secret.Annotations[corev1alpha1.ServiceAccountName]; saName != subjectName {
		return fmt.Errorf("incorrect subject name %s", saName)
	}
	return nil
}

func (r *Reconciler) checkServiceAccountRefPod(ctx context.Context, sa *corev1alpha1.ServiceAccount) error {
	if len(sa.Secrets) == 0 {
		klog.Warningf("service account %s has no secrets", sa.Name)
		return nil
	}
	pods := &v1.PodList{}
	if err := r.Client.List(ctx, pods, client.InNamespace(sa.Namespace)); err != nil {
		return err
	}

	saSecrets := sa.Secrets[0].Name
	for _, pod := range pods.Items {
		if pod.Annotations[AnnotationServiceAccountName] != sa.Name {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.Name == ServiceAccountVolumeName &&
				len(volume.Projected.Sources) > 0 &&
				saSecrets == volume.Projected.Sources[0].Secret.Name {
				continue
			}
		}
		if err := r.rolloutRestartPod(ctx, &pod); err != nil {
			return nil
		}
	}
	return nil
}

func (r *Reconciler) rolloutRestartPod(ctx context.Context, pod *v1.Pod) error {
	// check ownerReferences
	if len(pod.OwnerReferences) == 0 {
		klog.Infof("Pod has no owner references")
		return nil
	}

	owner := pod.OwnerReferences[0]
	switch owner.Kind {
	case "ReplicaSet":
		rs := &appsv1.ReplicaSet{}
		if err := r.Client.Get(ctx, types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      owner.Name,
		}, rs); err != nil {
			return err
		}
		if len(rs.OwnerReferences) > 0 && rs.OwnerReferences[0].Kind == "Deployment" {
			deployName := rs.OwnerReferences[0].Name
			deploy := &appsv1.Deployment{}
			if err := r.Client.Get(ctx, types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      deployName,
			}, deploy); err != nil {
				return err
			}
			if deploy.Spec.Template.ObjectMeta.Annotations == nil {
				deploy.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
			}
			deploy.Spec.Template.ObjectMeta.Annotations["kubesphere.io/restartedAt"] = metav1.Now().String()
			if err := r.Client.Update(ctx, deploy); err != nil {
				return err
			}
		}
	case "StatefulSet":
		sts := &appsv1.StatefulSet{}
		if err := r.Client.Get(ctx, types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      owner.Name,
		}, sts); err != nil {
			return err
		}
		if sts.Spec.Template.ObjectMeta.Annotations == nil {
			sts.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
		}
		sts.Spec.Template.ObjectMeta.Annotations["kubesphere.io/restartedAt"] = metav1.Now().String()
		if err := r.Client.Update(ctx, sts); err != nil {
			return err
		}
	case "DaemonSet":
		ds := &appsv1.DaemonSet{}
		if err := r.Client.Get(ctx, types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      owner.Name,
		}, ds); err != nil {
			return err
		}
		if ds.Spec.Template.ObjectMeta.Annotations == nil {
			ds.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
		}
		ds.Spec.Template.ObjectMeta.Annotations["kubesphere.io/restartedAt"] = metav1.Now().String()
		if err := r.Client.Update(ctx, ds); err != nil {
			return err
		}
	default:
		klog.Warningf("Unsupported owner kind %s", owner.Kind)
	}
	return nil
}
