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
	"github.com/versori-oss/nats-account-operator/controllers/resources"
	"k8s.io/client-go/tools/record"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	"github.com/versori-oss/nats-account-operator/api/accounts/v1alpha1"
	accountsclientsets "github.com/versori-oss/nats-account-operator/pkg/generated/clientset/versioned/typed/accounts/v1alpha1"
	"github.com/versori-oss/nats-account-operator/pkg/nsc"
)

// UserReconciler reconciles a User object
type UserReconciler struct {
	*BaseReconciler
	AccountsClientSet accountsclientsets.AccountsV1alpha1Interface
	EventRecorder     record.EventRecorder
}

//+kubebuilder:rbac:groups=accounts.nats.io,resources=users,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=accounts.nats.io,resources=users/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=accounts.nats.io,resources=users/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
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

	usr.Status.InitializeConditions()

	defer func() {
		if !equality.Semantic.DeepEqual(originalStatus, usr.Status) {
			if err2 := r.Status().Update(ctx, usr); err2 != nil {
				logger.Info("failed to update user status", "error", err2.Error())

				err = multierr.Append(err, err2)
			}
		}
	}()

	seed, ok, err := r.reconcileSeedSecret(ctx, usr)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}

	// get the KeyPairable which will be used to sign the JWT, resolveIssuer is part of BaseReconciler which doesn't
	// mark conditions (since it doesn't know what resource type it's reconciling), so we need to check for condition
	// errors and mark the conditions accordingly
	keyPairable, ok, err := r.resolveIssuer(ctx, usr.Spec.Issuer, usr.Namespace)
	if err != nil || !ok {
		if cerr, ok := asConditionError(err); ok {
			cerr.MarkCondition(usr.Status.MarkIssuerResolveFailed, usr.Status.MarkIssuerResolveUnknown)
		} else {
			usr.Status.MarkIssuerResolveUnknown(v1alpha1.ReasonUnknownError, err.Error())
		}

		if ok {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	_, ok, err = r.resolveAccount(ctx, usr, keyPairable)
	if err != nil || !ok {
		logger.Error(err, "failed to ensure owner resolved")

		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciling user JWT secret")

	ujwt, ok, err := r.reconcileJWTSecret(ctx, usr, keyPairable)
	if err != nil || !ok {
		logger.Error(err, "failed to reconcile user jwt secret")

		if ok {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciling user credential secret")

	if err := r.reconcileUserCredentialSecret(ctx, usr, ujwt, seed); err != nil {
		logger.Error(err, "failed to reconcile user credential secret")

		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileSeedSecret handles the v1alpha1.KeyPairableConditionSeedSecretReady condition. It ensures that a secret
// exists containing a valid keypair for the Account, updating it if it's not up-to-date and creating it if it doesn't
// exist.
func (r *UserReconciler) reconcileSeedSecret(ctx context.Context, usr *v1alpha1.User) (seed []byte, ok bool, err error) {
	logger := log.FromContext(ctx)

	got, err := r.CoreV1.Secrets(usr.Namespace).Get(ctx, usr.Spec.SeedSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("account seed secret not found, generating new keypair")

			return r.createSeedSecret(ctx, usr)
		}

		logger.Error(err, "failed to get account seed secret")

		usr.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return nil, false, err
	}

	kp, ok, err := r.ensureSeedSecretUpToDate(ctx, usr, got)
	if err != nil || !ok {
		if cerr, ok := asConditionError(err); ok {
			cerr.MarkCondition(usr.Status.MarkSeedSecretFailed, usr.Status.MarkSeedSecretUnknown)
		} else {
			usr.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())
		}

		return nil, ok, err
	}

	seedBytes, _ := kp.Seed()
	pubkey, _ := kp.PublicKey()

	usr.Status.MarkSeedSecretReady(pubkey, usr.Spec.SeedSecretName)

	return seedBytes, true, nil
}

func (r *UserReconciler) createSeedSecret(ctx context.Context, usr *v1alpha1.User) (seed []byte, ok bool, err error) {
	logger := log.FromContext(ctx)

	kp, err := nkeys.CreateUser()
	if err != nil {
		logger.Error(err, "failed to create user keypair")

		usr.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return nil, false, err
	}

	secret, err := resources.NewKeyPairSecretBuilder(r.Scheme).Build(usr, kp)
	if err != nil {
		logger.Error(err, "failed to build user keypair secret")

		usr.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return nil, false, err
	}

	if err := r.Client.Create(ctx, secret); err != nil {
		logger.Error(err, "failed to create user keypair secret")

		usr.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return nil, false, err
	}

	r.EventRecorder.Eventf(usr, v1.EventTypeNormal, "SeedSecretCreated", "created secret: %s/%s", secret.Namespace, secret.Name)

	pubkey, err := kp.PublicKey()
	if err != nil {
		logger.Error(err, "failed to get user public key")

		usr.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return nil, true, err
	}

	seedBytes, _ := kp.Seed()

	usr.Status.MarkSeedSecretReady(pubkey, secret.Name)

	return seedBytes, true, nil
}

// resolveAccount handles the v1alpha1.UserConditionAccountResolved condition and updating the
// .status.operatorRef field. If the provided keyPair is a SigningKey this will correctly resolve the owner to an
// Operator.
func (r *UserReconciler) resolveAccount(ctx context.Context, acc *v1alpha1.User, keyPair v1alpha1.KeyPairable) (account *v1alpha1.Account, ok bool, err error) {
	logger := log.FromContext(ctx)

	switch v := keyPair.(type) {
	case *v1alpha1.Account:
		logger.V(1).Info("user issuer is an account")

		account = v
	case *v1alpha1.SigningKey:
		logger.V(1).Info("user issuer is a signing key, resolving owner")

		owner, ok, err := r.resolveSigningKeyOwner(ctx, v)
		if err != nil || !ok {
			if cerr, ok := asConditionError(err); ok {
				cerr.MarkCondition(acc.Status.MarkAccountResolveFailed, acc.Status.MarkAccountResolveUnknown)
			} else {
				acc.Status.MarkAccountResolveUnknown(v1alpha1.ReasonUnknownError, err.Error())
			}

			if ok {
				return nil, true, nil
			}

			return nil, false, err
		}

		if account, ok = owner.(*v1alpha1.Account); !ok {
			acc.Status.MarkAccountResolveFailed(v1alpha1.ReasonInvalidSigningKeyOwner, "user issuer is not owned by an Account, got: %s", owner.GetObjectKind().GroupVersionKind().String())

			return nil, false, nil
		}
	default:
		logger.Info("invalid keypair, expected Account or SigningKey", "key_pair_type", fmt.Sprintf("%T", keyPair))

		acc.Status.MarkAccountResolveFailed(v1alpha1.ReasonUnsupportedIssuer, "invalid keypair, expected Account or SigningKey, got: %s", keyPair.GroupVersionKind().String())

		return nil, false, nil
	}

	acc.Status.MarkAccountResolved(v1alpha1.InferredObjectReference{
		Namespace: account.Namespace,
		Name:      account.Name,
	})

	return account, true, nil
}

func (r *UserReconciler) reconcileJWTSecret(ctx context.Context, usr *v1alpha1.User, keyPairable v1alpha1.KeyPairable) (string, bool, error) {
	logger := log.FromContext(ctx)

	issuerKP, ok, err := r.loadIssuerSeed(ctx, keyPairable, nkeys.PrefixByteAccount)
	if err != nil || !ok {
		if cerr, ok := asConditionError(err); ok {
			cerr.MarkCondition(usr.Status.MarkIssuerResolveFailed, usr.Status.MarkIssuerResolveUnknown)
		} else {
			usr.Status.MarkIssuerResolveUnknown(v1alpha1.ReasonUnknownError, err.Error())
		}

		return "", ok, err
	}

	usr.Status.MarkIssuerResolved()

	// we want to check that any existing secret decodes to match wantClaims, if it doesn't then we will use nextJWT
	// to create/update the secret. We cannot just compare the JWTs from the secret and accountJWT because the JWTs are
	// timestamped with the `iat` claim so will never match.
	wantClaims, nextJWT, err := nsc.CreateUserClaims(usr, issuerKP)
	if err != nil {
		usr.Status.MarkJWTSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return "", false, nil
	}

	got, err := r.CoreV1.Secrets(usr.Namespace).Get(ctx, usr.Spec.JWTSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("JWT secret not found, creating new secret")

			ok, err := r.createJWTSecret(ctx, usr, nextJWT)
			if err != nil || !ok {
				return "", ok, err
			}

			return nextJWT, true, nil
		}

		logger.Error(err, "failed to get JWT secret")

		usr.Status.MarkJWTSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return "", false, err
	}

	return r.ensureJWTSecretUpToDate(ctx, usr, wantClaims, got, nextJWT)
}

func (r *UserReconciler) createJWTSecret(ctx context.Context, usr *v1alpha1.User, userJWT string) (bool, error) {
	logger := log.FromContext(ctx)

	secret, err := resources.NewJWTSecretBuilder(r.Scheme).Build(usr, userJWT)
	if err != nil {
		logger.Error(err, "failed to build account keypair secret")

		usr.Status.MarkJWTSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return true, err
	}

	if err := r.Client.Create(ctx, secret); err != nil {
		logger.Error(err, "failed to create account JWT secret")

		usr.Status.MarkJWTSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		// we shouldn't expect any errors here, so returning false will communicate up to the controller that this
		// should be retried.
		return false, err
	}

	r.EventRecorder.Eventf(usr, v1.EventTypeNormal, "JWTSecretCreated", "created secret: %s/%s", secret.Namespace, secret.Name)

	return true, nil
}

// ensureJWTSecretUpToDate compares that the existing JWT secret decodes and matches the expected claims, if it does not
// match the secret will be updated with the nextJWT value.
func (r *UserReconciler) ensureJWTSecretUpToDate(ctx context.Context, usr *v1alpha1.User, wantClaims *jwt.UserClaims, got *v1.Secret, nextJWT string) (string, bool, error) {
	logger := log.FromContext(ctx)

	gotJWT, ok := got.Data[v1alpha1.NatsSecretJWTKey]
	if !ok {
		// TODO: should we be checking owner references here? If we own it then we should be okay to delete it, but if
		//  not we tell the user to delete it manually, and they'll either do so, or update the spec to use a new name.
		usr.Status.MarkJWTSecretFailed(v1alpha1.ReasonInvalidJWTSecret, "JWT secret does not contain JWT data, delete the secret to generate a new JWT")

		return "", false, nil
	}

	gotClaims, err := jwt.Decode(string(gotJWT))
	switch {
	case err != nil:
		logger.Info("failed to decode JWT from secret, updating to latest version", "reason", err.Error())
	case !nsc.Equality.DeepEqual(gotClaims, wantClaims):
		logger.V(1).Info("existing JWT secret does not match desired claims, updating to latest version")
	default:
		logger.V(1).Info("existing JWT secret matches desired claims, no update required")

		usr.Status.MarkJWTSecretReady()

		return string(gotJWT), true, nil
	}

	want, err := resources.NewJWTSecretBuilderFromSecret(got, r.Scheme).Build(usr, nextJWT)
	if err != nil {
		logger.Error(err, "failed to build desired JWT secret")

		usr.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return "", true, err
	}

	err = r.Client.Update(ctx, want)
	if err != nil {
		logger.Error(err, "failed to update JWT secret")

		usr.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return "", false, err
	}

	r.EventRecorder.Eventf(usr, v1.EventTypeNormal, "SeedSecretUpdated", "updated secret: %s/%s", want.Namespace, want.Name)

	usr.Status.MarkJWTSecretReady()

	return nextJWT, true, nil
}

func (r *UserReconciler) reconcileUserCredentialSecret(ctx context.Context, usr *v1alpha1.User, ujwt string, seed []byte) error {
	logger := log.FromContext(ctx)

	got, err := r.CoreV1.Secrets(usr.Namespace).Get(ctx, usr.Spec.CredentialsSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("credentials secret not found, creating new secret")

			if err := r.createCredentialsSecret(ctx, usr, ujwt, seed); err != nil {
				return err
			}

			return nil
		}

		logger.Error(err, "failed to get credentials secret")

		usr.Status.MarkCredentialsSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	return r.ensureCredentialsSecretUpToDate(ctx, usr, ujwt, seed, got)
}

func (r *UserReconciler) createCredentialsSecret(ctx context.Context, usr *v1alpha1.User, ujwt string, seed []byte) error {
	logger := log.FromContext(ctx)

	secret, err := resources.NewUserCredentialSecretBuilder(r.Scheme).Build(usr, ujwt, seed)
	if err != nil {
		logger.Error(err, "failed to build credentials secret")

		usr.Status.MarkCredentialsSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	if err := r.Client.Create(ctx, secret); err != nil {
		logger.Error(err, "failed to create credentials secret")

		usr.Status.MarkCredentialsSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	usr.Status.MarkCredentialsSecretReady()

	r.EventRecorder.Eventf(usr, v1.EventTypeNormal, "CredentialsSecretCreated", "created secret: %s/%s", secret.Namespace, secret.Name)

	return nil
}

func (r *UserReconciler) ensureCredentialsSecretUpToDate(ctx context.Context, usr *v1alpha1.User, ujwt string, seed []byte, got *v1.Secret) error {
	logger := log.FromContext(ctx)

	want, err := resources.NewUserCredentialSecretBuilderFromSecret(got.DeepCopy(), r.Scheme).Build(usr, ujwt, seed)
	if err != nil {
		err = fmt.Errorf("failed to build desired credentials secret: %w", err)

		usr.Status.MarkCredentialsSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	if equality.Semantic.DeepEqual(got, want) {
		logger.V(5).Info("existing credentials secret matches desired state, no update required")

		usr.Status.MarkCredentialsSecretReady()

		return nil
	}

	if err := r.Update(ctx, want); err != nil {
		err = fmt.Errorf("failed to update credentials secret: %w", err)

		usr.Status.MarkCredentialsSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	r.EventRecorder.Eventf(usr, v1.EventTypeNormal, "CredentialsSecretUpdated", "updated secret: %s/%s", want.Namespace, want.Name)

	usr.Status.MarkCredentialsSecretReady()

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *UserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.EventRecorder = mgr.GetEventRecorderFor("user-controller")

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.User{}).
		Owns(&v1.Secret{}).
		Complete(r)
}
