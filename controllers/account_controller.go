/*
MIT License

Copyright (c) 2022 Versori Ltd

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

*/

package controllers

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/nats-io/jwt"
	"github.com/nats-io/nkeys"
	accountsnatsiov1alpha1 "github.com/versori-oss/nats-account-operator/api/accounts/v1alpha1"
	accountsclientsets "github.com/versori-oss/nats-account-operator/pkg/generated/clientset/versioned/typed/accounts/v1alpha1"
	"github.com/versori-oss/nats-account-operator/pkg/nsc"
)

// AccountReconciler reconciles a Account object
type AccountReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	CV1Interface      corev1.CoreV1Interface
	AccountsClientSet accountsclientsets.AccountsV1alpha1Interface
}

//+kubebuilder:rbac:groups=accounts.nats.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=accounts.nats.io,resources=accounts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=accounts.nats.io,resources=accounts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Account object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *AccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	logger.V(1).Info("reconciling account", "account", req.Name)

	acc := new(accountsnatsiov1alpha1.Account)
	if err := r.Client.Get(ctx, req.NamespacedName, acc); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("account deleted")
			return ctrl.Result{}, nil
		}

		logger.Error(err, "failed to get account")
		return ctrl.Result{}, err
	}

	originalStatus := acc.Status.DeepCopy()

	defer func() {
		if err != nil {
			return
		}
		if !equality.Semantic.DeepEqual(originalStatus, acc.Status) {
			if err = r.Status().Update(ctx, acc); err != nil {
				if errors.IsConflict(err) {
					result.RequeueAfter = time.Second * 30
					return
				}
				logger.Error(err, "failed to update account status")

			}
		}
	}()

	opSKey, err := r.ensureOperatorResolved(ctx, acc)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("operator owner not found, requeuing")
			result.RequeueAfter = time.Second * 30
			return
		}
		logger.Error(err, "failed to ensure the operator owning the account was resolved")
		return ctrl.Result{}, err
	}

	sKeys, err := r.ensureSigningKeysUpdated(ctx, acc)
	if err != nil {
		if errors.IsNotFound(err) {
			result.RequeueAfter = time.Second * 30
			return
		}
		logger.Error(err, "failed to ensure signing keys were updated")
		return ctrl.Result{}, err
	}

	if err := r.ensureSeedJWTSecrets(ctx, acc, sKeys, opSKey); err != nil {
		logger.Error(err, "failed to ensure account jwt secret")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AccountReconciler) ensureSigningKeysUpdated(ctx context.Context, acc *accountsnatsiov1alpha1.Account) ([]accountsnatsiov1alpha1.SigningKeyEmbeddedStatus, error) {
	logger := log.FromContext(ctx)

	skList, err := r.AccountsClientSet.SigningKeys(acc.Namespace).List(ctx, metav1.ListOptions{})
	if err == nil && len(skList.Items) == 0 {
		logger.Info("no signing keys found")
		acc.Status.MarkSigningKeysUpdateUnknown("no signing keys found", "")
		return nil, errors.NewNotFound(accountsnatsiov1alpha1.Resource(accountsnatsiov1alpha1.SigningKey{}.ResourceVersion), "signingkeys")
	} else if err != nil {
		logger.Error(err, "failed to list signing keys")
		return nil, err
	}

	signingKeys := make([]accountsnatsiov1alpha1.SigningKeyEmbeddedStatus, 0)
	for _, sk := range skList.Items {
		if sk.Status.IsReady() && sk.Status.OwnerRef.Namespace == acc.Namespace && sk.Status.OwnerRef.Name == acc.Name {
			signingKeys = append(signingKeys, accountsnatsiov1alpha1.SigningKeyEmbeddedStatus{
				Name:    sk.GetName(),
				KeyPair: *sk.Status.KeyPair,
			})
		}
	}

	if len(signingKeys) == 0 {
		logger.V(1).Info("no ready signing keys found for account")
		acc.Status.MarkSigningKeysUpdateUnknown("no ready signing keys found for account", "account: %s", acc.Name)
		return nil, errors.NewNotFound(accountsnatsiov1alpha1.Resource(accountsnatsiov1alpha1.SigningKey{}.ResourceVersion), "signingkeys")
	}

	acc.Status.MarkSigningKeysUpdated(signingKeys)
	return signingKeys, nil
}

func (r *AccountReconciler) ensureOperatorResolved(ctx context.Context, acc *accountsnatsiov1alpha1.Account) ([]byte, error) {
	logger := log.FromContext(ctx)

	sKey, err := r.AccountsClientSet.SigningKeys(acc.Namespace).Get(ctx, acc.Spec.SigningKey.Ref.Name, metav1.GetOptions{})
	if err != nil {
		acc.Status.MarkOperatorResolveFailed("failed to get signing key reference for accout", "")
		logger.Info("failed to get signing key reference for account", "account: %s", acc.Name, "signing key name: %s", acc.Spec.SigningKey.Ref.Name)
		return []byte{}, err
	}

	if !sKey.Status.GetCondition(accountsnatsiov1alpha1.SigningKeyConditionOwnerResolved).IsTrue() {
		return []byte{}, errors.NewNotFound(accountsnatsiov1alpha1.Resource(accountsnatsiov1alpha1.SigningKey{}.ResourceVersion), sKey.Name)
	}

	skOwnerRef := sKey.Status.OwnerRef
	skOwnerRuntimeObj, _ := r.Scheme.New(skOwnerRef.GetGroupVersionKind())

	switch skOwnerRuntimeObj.(type) {
	case *accountsnatsiov1alpha1.Operator:
		acc.Status.MarkOperatorResolved(accountsnatsiov1alpha1.InferredObjectReference{
			Name:      skOwnerRef.Name,
			Namespace: skOwnerRef.Namespace,
		})
	default:
		acc.Status.MarkOperatorResolveFailed("invalid signing key owner type", "signing key type: %s", skOwnerRef.Kind)
		return []byte{}, errors.NewBadRequest("invalid signing key owner type")
	}

	skSeedSecret, err := r.CV1Interface.Secrets(acc.Namespace).Get(ctx, sKey.Status.KeyPair.SeedSecretName, metav1.GetOptions{})
	if err != nil {
		logger.Info("failed to get operator seed for signing key", "signing key: %s", sKey.Name, "operator: %s", skOwnerRef.Name)
		return []byte{}, err
	}

	return skSeedSecret.Data[accountsnatsiov1alpha1.NatsSecretSeedKey], nil
}

func (r *AccountReconciler) ensureSeedJWTSecrets(ctx context.Context, acc *accountsnatsiov1alpha1.Account, sKeys []accountsnatsiov1alpha1.SigningKeyEmbeddedStatus, opSkey []byte) error {
	logger := log.FromContext(ctx)

	natsHelper := nsc.NscHelper{
		OperatorRef:  acc.Status.OperatorRef,
		CV1Interface: r.CV1Interface,
		AccClientSet: r.AccountsClientSet,
	}

	_, errSeed := r.CV1Interface.Secrets(acc.Namespace).Get(ctx, acc.Spec.SeedSecretName, metav1.GetOptions{})
	jwtSec, errJWT := r.CV1Interface.Secrets(acc.Namespace).Get(ctx, acc.Spec.JWTSecretName, metav1.GetOptions{})

	var sKeysPublicKeys []string
	for _, sk := range sKeys {
		sKeysPublicKeys = append(sKeysPublicKeys, sk.KeyPair.PublicKey)
	}

	if errors.IsNotFound(errSeed) || errors.IsNotFound(errJWT) {
		accClaims := jwt.Account{
			Imports:     nsc.ConvertToNATSImports(acc.Spec.Imports),
			Exports:     nsc.ConvertToNATSExports(acc.Spec.Exports),
			Identities:  nsc.ConvertToNATSIdentities(acc.Spec.Identities),
			Limits:      nsc.ConvertToNATSOperatorLimits(acc.Spec.Limits),
			SigningKeys: sKeysPublicKeys,
		}

		kPair, err := nkeys.FromSeed(opSkey)
		if err != nil {
			logger.Error(err, "failed to make key pair from seed")
			return err
		}

		ajwt, publicKey, seed, err := nsc.CreateAccount(acc.Name, accClaims, kPair)
		if err != nil {
			logger.Error(err, "failed to create account")
			return err
		}

		jwtData := map[string][]byte{accountsnatsiov1alpha1.NatsSecretJWTKey: []byte(ajwt)}
		jwtSecret := NewSecret(acc.Spec.JWTSecretName, acc.Namespace, WithData(jwtData), WithImmutable(false))
		if err := ctrl.SetControllerReference(acc, &jwtSecret, r.Scheme); err != nil {
			logger.Error(err, "failed to set account as owner of jwt secret")
			return err
		}

		if _, err := createOrUpdateSecret(ctx, r.CV1Interface, acc.Namespace, &jwtSecret, !errors.IsNotFound(errJWT)); err != nil {
			logger.Error(err, "failed to create or update jwt secret")
			return err
		}

		seedData := map[string][]byte{accountsnatsiov1alpha1.NatsSecretSeedKey: seed}
		seedSecret := NewSecret(acc.Spec.SeedSecretName, acc.Namespace, WithData(seedData), WithImmutable(true))
		if err := ctrl.SetControllerReference(acc, &seedSecret, r.Scheme); err != nil {
			logger.Error(err, "failed to set account as owner of seed secret")
			return err
		}

		if _, err := createOrUpdateSecret(ctx, r.CV1Interface, acc.Namespace, &seedSecret, !errors.IsNotFound(errSeed)); err != nil {
			logger.Error(err, "failed to create or update seed secret")
			return err
		}

		if isSys, err := r.isSystemAccount(ctx, acc); err == nil && !isSys {
			err = natsHelper.PushJWT(ctx, ajwt)
			if err != nil {
				logger.Info("failed to push account jwt to nats server", "error", err)
				acc.Status.MarkJWTPushFailed("failed to push account jwt to nats server", "error: %s", err)
				return nil
			}
		} else if err != nil {
			logger.Error(err, "failed to determine if account is system account")
			return err
		}

		acc.Status.MarkJWTPushed()
		acc.Status.MarkJWTSecretReady()
		acc.Status.MarkSeedSecretReady(publicKey, seedSecret.Name)
		return nil
	} else if errSeed != nil {
		// logging and returning errors here since something could have actually gone wrong
		acc.Status.MarkSeedSecretUnknown("failed to get seed secret", "")
		logger.Error(errSeed, "failed to get seed secret")
		return errSeed
	} else if errJWT != nil {
		acc.Status.MarkJWTSecretUnknown("failed to get jwt secret", "")
		acc.Status.MarkJWTPushUnknown("failed to get jwt secret", "")
		logger.Error(errJWT, "failed to get jwt secret")
		return errJWT
	} else {
		// err := natsPusher.UpdateJWT(ctx, )
		err := r.updateAccountJWTSigningKeys(ctx, acc, opSkey, jwtSec, sKeysPublicKeys, &natsHelper)
		if err != nil {
			logger.V(1).Info("failed to update account JWT with signing keys", "error", err)
			acc.Status.MarkJWTPushUnknown("failed to update account JWT with signing keys", "")
			acc.Status.MarkJWTSecretUnknown("failed to update account JWT with signing keys", "")
			return nil
		}
	}

	return nil
}

func (r *AccountReconciler) updateAccountJWTSigningKeys(ctx context.Context, acc *accountsnatsiov1alpha1.Account, operatorSeed []byte, jwtSecret *v1.Secret, sKeys []string, natsHelper nsc.NSCInterface) error {
	logger := log.FromContext(ctx)

	accJWTEncoded := string(jwtSecret.Data[accountsnatsiov1alpha1.NatsSecretJWTKey])
	accClaims, err := jwt.DecodeAccountClaims(accJWTEncoded)
	if err != nil {
		logger.Error(err, "failed to decode account jwt")
		return err
	}

	if isEqualUnordered(accClaims.SigningKeys, sKeys) {
		logger.V(1).Info("account jwt signing keys are already up to date")
		return nil
	}

	accClaims.SigningKeys = jwt.StringList(sKeys)

	kPair, err := nkeys.FromSeed(operatorSeed)
	if err != nil {
		logger.Error(err, "failed to create nkeys from operator seed")
		return err
	}
	ajwt, err := accClaims.Encode(kPair)
	if err != nil {
		logger.Error(err, "failed to encode account jwt")
		return err
	}

	jwtSecret.Data[accountsnatsiov1alpha1.NatsSecretJWTKey] = []byte(ajwt)
	_, err = r.CV1Interface.Secrets(jwtSecret.Namespace).Update(ctx, jwtSecret, metav1.UpdateOptions{})
	if err != nil {
		logger.Error(err, "failed to update jwt secret")
		return err
	}

	if isSys, err := r.isSystemAccount(ctx, acc); err == nil && !isSys {
		err = natsHelper.UpdateJWT(ctx, accClaims.Subject, ajwt)
		if err != nil {
			logger.Error(err, "failed to update account jwt on nats server")
			return err
		}
	}

	return nil
}

// isSystemAccount returns true if acc is the system account this can only be used if the accounts operator has been resolved.
func (r *AccountReconciler) isSystemAccount(ctx context.Context, acc *accountsnatsiov1alpha1.Account) (bool, error) {
	logger := log.FromContext(ctx)

	if !acc.Status.GetCondition(accountsnatsiov1alpha1.AccountConditionOperatorResolved).IsTrue() {
		logger.V(1).Info("operator not resolved for account")
		// just return true here because we DO NOT push when true, so essentially wait for next reconcile when operator is resolved
		return true, nil
	}
	// the account is the system account if and only if the system account, in the operator CR referenced by account have the same name and namespace.
	operator, err := r.AccountsClientSet.Operators(acc.Status.OperatorRef.Namespace).Get(ctx, acc.Status.OperatorRef.Name, metav1.GetOptions{})
	if err != nil {
		return true, err
	}

	if !operator.Status.GetCondition(accountsnatsiov1alpha1.OperatorConditionSystemAccountResolved).IsTrue() {
		logger.V(1).Info("system account not yet resolved for operator")
		return true, nil
	}

	if acc.Name == operator.Status.ResolvedSystemAccount.Name && acc.Namespace == operator.Status.ResolvedSystemAccount.Namespace {
		return true, nil
	}

	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := mgr.GetLogger().WithName("AccountReconciler")
	err := ctrl.NewControllerManagedBy(mgr).
		For(&accountsnatsiov1alpha1.Account{}).
		Owns(&v1.Secret{}).
		Watches(
			&source.Kind{Type: &accountsnatsiov1alpha1.SigningKey{}},
			handler.EnqueueRequestsFromMapFunc(func(obj client.Object) []reconcile.Request {
				signingKey, ok := obj.(*accountsnatsiov1alpha1.SigningKey)
				if !ok {
					logger.Info("SigningKey watcher received non-SigningKey object",
						"kind", obj.GetObjectKind().GroupVersionKind().String())
					return nil
				}

				ownerRef := signingKey.Status.OwnerRef
				if ownerRef == nil {
					return nil
				}

				accountGVK := accountsnatsiov1alpha1.GroupVersion.WithKind("Account")
				if accountGVK != ownerRef.GetGroupVersionKind() {
					return nil
				}

				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      ownerRef.Name,
						Namespace: ownerRef.Namespace,
					},
				}}
			}),
		).
		Complete(r)

	if err != nil {
		return err
	}

	return nil
}
