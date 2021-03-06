/*
Copyright 2020 Clastix Labs.

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

package secret

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/clastix/capsule/pkg/cert"
)

type CaReconciler struct {
	client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	Namespace string
}

func (r *CaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}, forOptionPerInstanceName(caSecretName)).
		Complete(r)
}

func (r CaReconciler) UpdateValidatingWebhookConfiguration(wg *sync.WaitGroup, ch chan error, caBundle []byte) {
	defer wg.Done()

	var err error

	ch <- retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		vw := &v1.ValidatingWebhookConfiguration{}
		err = r.Get(context.TODO(), types.NamespacedName{Name: "capsule-validating-webhook-configuration"}, vw)
		if err != nil {
			r.Log.Error(err, "cannot retrieve ValidatingWebhookConfiguration")
			return err
		}
		for i, w := range vw.Webhooks {
			// Updating CABundle only in case of an internal service reference
			if w.ClientConfig.Service != nil {
				vw.Webhooks[i].ClientConfig.CABundle = caBundle
			}
		}
		return r.Update(context.TODO(), vw, &client.UpdateOptions{})
	})
}

func (r CaReconciler) UpdateMutatingWebhookConfiguration(wg *sync.WaitGroup, ch chan error, caBundle []byte) {
	defer wg.Done()

	var err error

	ch <- retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		mw := &v1.MutatingWebhookConfiguration{}
		err = r.Get(context.TODO(), types.NamespacedName{Name: "capsule-mutating-webhook-configuration"}, mw)
		if err != nil {
			r.Log.Error(err, "cannot retrieve MutatingWebhookConfiguration")
			return err
		}
		for i, w := range mw.Webhooks {
			// Updating CABundle only in case of an internal service reference
			if w.ClientConfig.Service != nil {
				mw.Webhooks[i].ClientConfig.CABundle = caBundle
			}
		}
		return r.Update(context.TODO(), mw, &client.UpdateOptions{})
	})
}

func (r CaReconciler) Reconcile(request ctrl.Request) (ctrl.Result, error) {
	var err error

	r.Log = r.Log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	r.Log.Info("Reconciling CA Secret")

	// Fetch the CA instance
	instance := &corev1.Secret{}
	err = r.Client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	var ca cert.Ca
	var rq time.Duration
	ca, err = getCertificateAuthority(r.Client, r.Namespace)
	if err != nil && errors.Is(err, MissingCaError{}) {
		ca, err = cert.GenerateCertificateAuthority()
		if err != nil {
			return reconcile.Result{}, err
		}
	} else if err != nil {
		return reconcile.Result{}, err
	}

	r.Log.Info("Handling CA Secret")

	rq, err = ca.ExpiresIn(time.Now())
	if err != nil {
		r.Log.Info("CA is expired, cleaning to obtain a new one")
		instance.Data = map[string][]byte{}
	} else {
		r.Log.Info("Updating CA secret with new PEM and RSA")

		var crt *bytes.Buffer
		var key *bytes.Buffer
		crt, _ = ca.CaCertificatePem()
		key, _ = ca.CaPrivateKeyPem()

		instance.Data = map[string][]byte{
			certSecretKey:       crt.Bytes(),
			privateKeySecretKey: key.Bytes(),
		}

		wg := &sync.WaitGroup{}
		wg.Add(2)
		ch := make(chan error, 2)

		go r.UpdateMutatingWebhookConfiguration(wg, ch, crt.Bytes())
		go r.UpdateValidatingWebhookConfiguration(wg, ch, crt.Bytes())

		wg.Wait()
		close(ch)

		for err = range ch {
			if err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	var res controllerutil.OperationResult
	t := &corev1.Secret{ObjectMeta: instance.ObjectMeta}
	res, err = controllerutil.CreateOrUpdate(context.TODO(), r.Client, t, func() error {
		t.Data = instance.Data
		return nil
	})
	if err != nil {
		r.Log.Error(err, "cannot update Capsule TLS")
		return reconcile.Result{}, err
	}

	if res == controllerutil.OperationResultUpdated {
		r.Log.Info("Capsule CA has been updated, we need to trigger TLS update too")
		tls := &corev1.Secret{}
		err = r.Get(context.TODO(), types.NamespacedName{
			Namespace: r.Namespace,
			Name:      tlsSecretName,
		}, tls)
		if err != nil {
			r.Log.Error(err, "Capsule TLS Secret missing")
		}
		err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			_, err = controllerutil.CreateOrUpdate(context.TODO(), r.Client, tls, func() error {
				tls.Data = map[string][]byte{}
				return nil
			})
			return err
		})
		if err != nil {
			r.Log.Error(err, "Cannot clean Capsule TLS Secret due to CA update")
			return reconcile.Result{}, err
		}
	}

	r.Log.Info("Reconciliation completed, processing back in " + rq.String())
	return reconcile.Result{Requeue: true, RequeueAfter: rq}, nil
}
