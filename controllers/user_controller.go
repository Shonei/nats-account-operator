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
	"fmt"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	"github.com/versori-oss/nats-account-operator/api/accounts/v1alpha1"
	"github.com/versori-oss/nats-account-operator/pkg/apis"
	accountsclientsets "github.com/versori-oss/nats-account-operator/pkg/generated/clientset/versioned/typed/accounts/v1alpha1"
	"github.com/versori-oss/nats-account-operator/pkg/nsc"
)

// UserReconciler reconciles a User object
type UserReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	CV1Interface      corev1.CoreV1Interface
	AccountsClientSet accountsclientsets.AccountsV1alpha1Interface
}

//+kubebuilder:rbac:groups=accounts.nats.io,resources=users,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=accounts.nats.io,resources=users/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=accounts.nats.io,resources=users/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the User object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *UserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	usr := new(v1alpha1.User)
	if err := r.Client.Get(ctx, req.NamespacedName, usr); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("user deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to fetch user")
		return ctrl.Result{}, err
	}

	originalStatus := usr.Status.DeepCopy()

	defer func() {
		if !equality.Semantic.DeepEqual(originalStatus, usr.Status) {
			if err2 := r.Status().Update(ctx, usr); err2 != nil {
				logger.Info("failed to update user status", "error", err2.Error())

				err = multierr.Append(err, err2)
			}
		}
	}()

	accKeyPair, err := r.ensureAccountResolved(ctx, usr)
	if err != nil {
		logger.Error(err, "failed to ensure owner resolved")
		return ctrl.Result{}, err
	}

	if err := r.ensureCredsSecrets(ctx, usr, accKeyPair); err != nil {
		logger.Error(err, "failed to ensure JWT seed secrets")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *UserReconciler) ensureAccountResolved(ctx context.Context, usr *v1alpha1.User) (*v1alpha1.KeyPair, error) {
	// logger := log.FromContext(ctx)

	skRef := usr.Spec.SigningKey.Ref
	skGVK := skRef.GetGroupVersionKind()
	obj, err := r.Scheme.New(skGVK)
	if err != nil {
		usr.Status.MarkAccountResolveFailed(
			v1alpha1.ReasonUnsupportedSigningKey, "unsupported GroupVersionKind: %s", err.Error())
		return nil, err
	}

	signingKey, ok := obj.(client.Object)
	if !ok {
		usr.Status.MarkAccountResolveFailed(
			v1alpha1.ReasonUnsupportedSigningKey, "runtime.Object cannot be converted to client.Object", skGVK)
		return nil, err
	}

	ns := skRef.Namespace
	if ns == "" {
		ns = usr.Namespace
	}

	err = r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: skRef.Name}, signingKey)
	if err != nil {
		if errors.IsNotFound(err) {
			usr.Status.MarkAccountResolveFailed(
				v1alpha1.ReasonNotFound, "%s, %s/%s: not found", skGVK, ns, skRef.Name)
		}
		return nil, err
	}

	conditionAccessor, ok := signingKey.(apis.ConditionManagerAccessor)
	if !ok {
		usr.Status.MarkAccountResolveFailed(
			v1alpha1.ReasonUnsupportedSigningKey,
			"%s does not implement ConditionAccessor: %T",
			signingKey.GetObjectKind().GroupVersionKind(),
			signingKey)
		return nil, fmt.Errorf("signing key ref does not implement ConditionAccessor: %T", signingKey)
	}

	if !conditionAccessor.GetConditionManager().IsHappy() {
		usr.Status.MarkAccountResolveUnknown(v1alpha1.ReasonNotReady, "signing key not ready")

		return nil, fmt.Errorf("signing key not ready")
	}

	var account *v1alpha1.Account

	switch owner := signingKey.(type) {
	case *v1alpha1.Account:
		account = owner
	case *v1alpha1.SigningKey:
		account, err = r.lookupAccountForSigningKey(ctx, owner)
		if err != nil {
			return nil, err
		}
	default:
		usr.Status.MarkAccountResolveFailed(
			v1alpha1.ReasonUnsupportedSigningKey,
			"expected Account or SigningKey but got: %T",
			signingKey)
		return nil, fmt.Errorf("expected Account or SigningKey but got: %T", signingKey)
	}

	usr.Status.MarkAccountResolved(v1alpha1.InferredObjectReference{
		Namespace: account.Namespace,
		Name:      account.Name,
	})

	keyPaired, ok := signingKey.(v1alpha1.KeyPairAccessor)
	if !ok {
		return nil, fmt.Errorf("signing key ref does not implement KeyPairAccessor: %T", signingKey)
	}

	keyPair := keyPaired.GetKeyPair()
	if keyPair == nil {
		return nil, fmt.Errorf("signing key ref does not have a key pair")
	}

	return keyPair, nil
}

func (r *UserReconciler) lookupAccountForSigningKey(ctx context.Context, sk *v1alpha1.SigningKey) (*v1alpha1.Account, error) {
	ownerRef := sk.Status.OwnerRef

	if ownerRef == nil {
		return nil, fmt.Errorf("signing key %s/%s has no owner reference", sk.Namespace, sk.Name)
	}

	return r.AccountsClientSet.Accounts(ownerRef.Namespace).Get(ctx, ownerRef.Name, metav1.GetOptions{})
}

func (r *UserReconciler) ensureCredsSecrets(ctx context.Context, usr *v1alpha1.User, keyPair *v1alpha1.KeyPair) error {
	logger := log.FromContext(ctx)

	accSKeySecret, err := r.CV1Interface.Secrets(usr.Spec.SigningKey.Ref.Namespace).Get(ctx, keyPair.SeedSecretName, metav1.GetOptions{})
	if err != nil {
		logger.Error(err, "failed to get users signing key seed secret")
		return err
	}
	accSKey := accSKeySecret.Data[v1alpha1.NatsSecretSeedKey]

	_, errSeed := r.CV1Interface.Secrets(usr.Namespace).Get(ctx, usr.Spec.SeedSecretName, metav1.GetOptions{})
	_, errJWT := r.CV1Interface.Secrets(usr.Namespace).Get(ctx, usr.Spec.JWTSecretName, metav1.GetOptions{})
	_, errCreds := r.CV1Interface.Secrets(usr.Namespace).Get(ctx, usr.Spec.CredentialsSecretName, metav1.GetOptions{})
	if errors.IsNotFound(errSeed) || errors.IsNotFound(errJWT) || errors.IsNotFound(errCreds) {
		// one or the other is not found, so re-create the creds, seed and jwt and then update/create the secrets

		kPair, err := nkeys.ParseDecoratedNKey(accSKey)
		if err != nil {
			logger.Error(err, "failed to make key pair from seed")
			return err
		}

		usrClaims := jwt.User{
			UserPermissionLimits: jwt.UserPermissionLimits{
				Permissions:            nsc.ConvertToNATSUserPermissions(usr.Spec.Permissions),
				Limits:                 nsc.ConvertToNATSLimits(usr.Spec.Limits),
				BearerToken:            usr.Spec.BearerToken,
				AllowedConnectionTypes: []string{},
			},
			IssuerAccount: keyPair.PublicKey,
			GenericFields: jwt.GenericFields{
				Tags:    []string{},
				Type:    "",
				Version: 0,
			},
		}

		ujwt, publicKey, seed, err := nsc.CreateUser(usr.Name, usrClaims, kPair)
		if err != nil {
			logger.Error(err, "failed to create user jwt")
			return err
		}

		userCreds, err := jwt.FormatUserConfig(ujwt, seed)
		if err != nil {
			logger.Info("failed to format user creds")
			usr.Status.MarkCredentialsSecretFailed("failed to format user creds", "")
			return nil
		}

		// create jwt secret and update or create it on the cluster
		jwtData := map[string][]byte{v1alpha1.NatsSecretJWTKey: []byte(ujwt)}
		jwtSecret := NewSecret(usr.Spec.JWTSecretName, usr.Namespace, WithData(jwtData), WithImmutable(true))
		if err = ctrl.SetControllerReference(usr, &jwtSecret, r.Scheme); err != nil {
			logger.Error(err, "failed to set user as owner of jwt secret")
			return err
		}

		if _, err := createOrUpdateSecret(ctx, r.CV1Interface, usr.Namespace, &jwtSecret, !errors.IsNotFound(errJWT)); err != nil {
			logger.Error(err, "failed to create or update jwt secret")
			return err
		}

		// create seed secret and update or create it on the cluster
		seedData := map[string][]byte{
			v1alpha1.NatsSecretSeedKey:      seed,
			v1alpha1.NatsSecretPublicKeyKey: []byte(publicKey),
		}
		seedSecret := NewSecret(usr.Spec.SeedSecretName, usr.Namespace, WithData(seedData), WithImmutable(true))
		if err = ctrl.SetControllerReference(usr, &seedSecret, r.Scheme); err != nil {
			logger.Error(err, "failed to set user as owner of seed secret")
			return err
		}

		if _, err := createOrUpdateSecret(ctx, r.CV1Interface, usr.Namespace, &seedSecret, !errors.IsNotFound(errSeed)); err != nil {
			logger.Error(err, "failed to create or update seed secret")
			return err
		}

		// create creds secret and update or create it on the cluster
		credsData := map[string][]byte{v1alpha1.NatsSecretCredsKey: userCreds}
		credsSecret := NewSecret(usr.Spec.CredentialsSecretName, usr.Namespace, WithData(credsData), WithImmutable(false))
		if err = ctrl.SetControllerReference(usr, &credsSecret, r.Scheme); err != nil {
			logger.Error(err, "failed to set user as owner of creds secret")
			return err
		}

		if _, err := createOrUpdateSecret(ctx, r.CV1Interface, usr.Namespace, &credsSecret, !errors.IsNotFound(errCreds)); err != nil {
			logger.Error(err, "failed to create or update creds secret")
			return err
		}

		usr.Status.MarkCredentialsSecretReady()
		usr.Status.MarkJWTSecretReady()
		usr.Status.MarkSeedSecretReady(publicKey, usr.Spec.SeedSecretName)
		return nil
	} else if errSeed != nil {
		// going to actually return and log errors here as something could have gone genuinely wrong
		logger.Error(errSeed, "failed to get seed secret")
		usr.Status.MarkSeedSecretUnknown("failed to get seed secret", "")
		return errSeed
	} else if errJWT != nil {
		logger.Error(errJWT, "failed to get jwt secret")
		usr.Status.MarkJWTSecretUnknown("failed to get jwt secret", "")
		return errJWT
	} else if errCreds != nil {
		logger.Error(errCreds, "failed to get credentials secret")
		usr.Status.MarkCredentialsSecretUnknown("failed to get credentials secrets", "")
		return errCreds
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *UserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.User{}).
		Owns(&v1.Secret{}).
		Complete(r)
}
