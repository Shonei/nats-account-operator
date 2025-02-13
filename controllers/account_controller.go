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
	"github.com/nats-io/nats.go"
	"github.com/versori-oss/nats-account-operator/controllers/resources"
	"github.com/versori-oss/nats-account-operator/pkg/helpers"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	"github.com/versori-oss/nats-account-operator/api/accounts/v1alpha1"
	accountsclientsets "github.com/versori-oss/nats-account-operator/pkg/generated/clientset/versioned/typed/accounts/v1alpha1"
	"github.com/versori-oss/nats-account-operator/pkg/nsc"
)

const AccountFinalizer = "accounts.nats.io/finalizer"

// AccountReconciler reconciles an Account object
type AccountReconciler struct {
	*BaseReconciler
	AccountsV1Alpha1 accountsclientsets.AccountsV1alpha1Interface
	SysAccountLoader *nsc.SystemAccountLoader
}

//+kubebuilder:rbac:groups=accounts.nats.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=accounts.nats.io,resources=accounts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=accounts.nats.io,resources=accounts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *AccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	logger.V(1).Info("reconciling account", "account", req.Name)
	acc := new(v1alpha1.Account)
	if err := r.Client.Get(ctx, req.NamespacedName, acc); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("account deleted")
			return ctrl.Result{}, nil
		}

		logger.Error(err, "failed to get account")
		return ctrl.Result{}, err
	}

	originalStatus := acc.Status.DeepCopy()

	acc.Status.InitializeConditions()

	defer func() {
		if !equality.Semantic.DeepEqual(originalStatus, acc.Status) {
			if err2 := r.Status().Update(ctx, acc); err2 != nil {
				logger.Info("failed to update account status", "error", err2.Error(), "account_name", acc.Name, "account_namespace", acc.Namespace)

				err = multierr.Append(err, err2)
			}
		}
	}()

	if acc.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(acc, AccountFinalizer) {
			controllerutil.AddFinalizer(acc, AccountFinalizer)
			if err := r.Update(ctx, acc); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(acc, AccountFinalizer) {
			if err := r.finalizeAccount(ctx, acc); err != nil {
				return ctrl.Result{}, err
			}

			logger.V(1).Info("account successfully finalized")

			controllerutil.RemoveFinalizer(acc, AccountFinalizer)
			if err := r.Update(ctx, acc); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	}

	ok, err := r.reconcileSeedSecret(ctx, acc)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}

	// get the KeyPairable which will be used to sign the Account JWT
	keyPairable, ok, err := r.resolveIssuer(ctx, acc.Spec.Issuer, acc.Namespace)
	if err != nil || !ok {
		if cerr, ok := asConditionError(err); ok {
			cerr.MarkCondition(acc.Status.MarkIssuerResolveFailed, acc.Status.MarkIssuerResolveUnknown)
		} else {
			acc.Status.MarkIssuerResolveUnknown(v1alpha1.ReasonUnknownError, err.Error())
		}

		if ok {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	operator, ok, err := r.resolveOperator(ctx, acc, keyPairable)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}

	// make sure signing keys for this Account are up-to-date before we try to sign the JWT
	err = r.ensureSigningKeysUpdated(ctx, acc)
	if err != nil {
		logger.Info("failed to ensure signing keys were updated", "error", err.Error())

		return ctrl.Result{}, err
	}

	issuerKP, ok, err := r.loadIssuerSeed(ctx, acc, keyPairable)
	if err != nil || !ok {
		if ok {
			logger.Info("cluster not prepared for loading the issuer seed, will try later", "error", err.Error())

			// something else needs to change which will trigger another reconcile
			return ctrl.Result{}, nil
		}

		logger.Error(err, "failed to load issuer seed")

		return ctrl.Result{}, err
	}

	accountJWT, ok, err := r.reconcileJWTSecret(ctx, acc, issuerKP)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}

	if !helpers.IsSystemAccount(acc, operator) {
		if err := r.ensureJWTPushed(ctx, acc, operator, issuerKP, accountJWT); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		// system accounts need the condition to be set to true to enable them to be considered "ready"
		acc.Status.MarkJWTPushed()
	}

	return ctrl.Result{}, nil
}

// resolveOperator handles the v1alpha1.AccountConditionOperatorResolved condition and updating the
// .status.operatorRef field. If the provided keyPair is a SigningKey this will correctly resolve the owner to an
// Operator.
func (r *AccountReconciler) resolveOperator(ctx context.Context, acc *v1alpha1.Account, keyPair v1alpha1.KeyPairable) (operator *v1alpha1.Operator, ok bool, err error) {
	logger := log.FromContext(ctx)

	switch v := keyPair.(type) {
	case *v1alpha1.Operator:
		logger.V(1).Info("account issuer is an operator")

		operator = v
	case *v1alpha1.SigningKey:
		logger.V(1).Info("account issuer is a signing key, resolving operator")

		owner, ok, err := r.resolveSigningKeyOwner(ctx, v)
		if err != nil || !ok {
			if cerr, ok := asConditionError(err); ok {
				cerr.MarkCondition(acc.Status.MarkOperatorResolveFailed, acc.Status.MarkOperatorResolveUnknown)
			} else {
				acc.Status.MarkOperatorResolveUnknown(v1alpha1.ReasonUnknownError, err.Error())
			}

			if ok {
				return nil, true, nil
			}

			return nil, false, err
		}

		if operator, ok = owner.(*v1alpha1.Operator); !ok {
			acc.Status.MarkOperatorResolveFailed(v1alpha1.ReasonInvalidSigningKeyOwner, "account issuer is not owned by an Operator, got: %s", owner.GetObjectKind().GroupVersionKind().String())

			return nil, false, nil
		}
	default:
		logger.Info("invalid keypair, expected Operator or SigningKey", "key_pair_type", fmt.Sprintf("%T", keyPair))

		acc.Status.MarkOperatorResolveFailed(v1alpha1.ReasonUnsupportedIssuer, "invalid keypair, expected Operator or SigningKey, got: %s", keyPair.GroupVersionKind().String())

		return nil, false, nil
	}

	acc.Status.MarkOperatorResolved(v1alpha1.InferredObjectReference{
		Namespace: operator.Namespace,
		Name:      operator.Name,
	})

	return operator, true, nil
}

func (r *AccountReconciler) resolveSigningKeyOperator(ctx context.Context, acc *v1alpha1.Account, sk *v1alpha1.SigningKey) (*v1alpha1.Operator, bool, error) {
	logger := log.FromContext(ctx)

	if !sk.Status.GetCondition(v1alpha1.SigningKeyConditionOwnerResolved).IsTrue() {
		acc.Status.MarkOperatorResolveUnknown(v1alpha1.ReasonNotReady, "signing key owner has not been resolved")

		return nil, false, nil
	}

	// TODO: we should load a runtime.Object from the scheme and Get that, then check that the OwnerRef is an Operator.
	//  Right now if a SigningKey is owned by a non-Operator, we'll fail with a "not found" error even though the
	//  OwnerRef is actually set.
	operator := new(v1alpha1.Operator)

	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: sk.Status.OwnerRef.Namespace,
		Name:      sk.Status.OwnerRef.Name,
	}, operator)
	if err != nil {
		logger.Error(err, "failed to get signing key owner")

		if errors.IsNotFound(err) {
			acc.Status.MarkOperatorResolveFailed(v1alpha1.ReasonNotFound, err.Error())

			return nil, false, nil
		}

		acc.Status.MarkOperatorResolveUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return nil, false, err
	}

	return operator, true, nil
}

// reconcileSeedSecret handles the v1alpha1.KeyPairableConditionSeedSecretReady condition. It ensures that a secret
// exists containing a valid keypair for the Account, updating it if it's not up-to-date and creating it if it doesn't
// exist.
func (r *AccountReconciler) reconcileSeedSecret(ctx context.Context, acc *v1alpha1.Account) (bool, error) {
	logger := log.FromContext(ctx)

	got, err := r.CoreV1.Secrets(acc.Namespace).Get(ctx, acc.Spec.SeedSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("account seed secret not found, generating new keypair")

			return true, r.createSeedSecret(ctx, acc)
		}

		logger.Error(err, "failed to get account seed secret")

		acc.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return false, err
	}

	return r.ensureSeedSecretUpToDate(ctx, acc, got)
}

func (r *AccountReconciler) createSeedSecret(ctx context.Context, acc *v1alpha1.Account) error {
	logger := log.FromContext(ctx)

	kp, err := nkeys.CreateAccount()
	if err != nil {
		logger.Error(err, "failed to create account keypair")

		acc.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	secret, err := resources.NewKeyPairSecretBuilder(r.Scheme).Build(acc, kp)
	if err != nil {
		logger.Error(err, "failed to build account keypair secret")

		acc.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

    if err = controllerutil.SetControllerReference(acc, secret, r.Scheme); err != nil {
        logger.Error(err, "failed to set account keypair secret controller reference")

        acc.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

        return err
    }

    if err := r.Client.Create(ctx, secret); err != nil {
		logger.Error(err, "failed to create account keypair secret")

		acc.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	r.EventRecorder.Eventf(acc, v1.EventTypeNormal, "SeedSecretCreated", "created secret: %s/%s", secret.Namespace, secret.Name)

	pubkey, err := kp.PublicKey()
	if err != nil {
		logger.Error(err, "failed to get account public key")

		acc.Status.MarkSeedSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	acc.Status.MarkSeedSecretReady(pubkey, secret.Name)

	return nil
}

func (r *AccountReconciler) ensureSeedSecretUpToDate(ctx context.Context, acc *v1alpha1.Account, got *v1.Secret) (bool, error) {
	logger := log.FromContext(ctx)

	seed, ok := got.Data[v1alpha1.NatsSecretSeedKey]
	if !ok {
		// TODO: should we be checking owner references here? If we own it then we should be okay to delete it, but if
		//  not we tell the user to delete it manually, and they'll either do so, or update the spec to use a new name.
		acc.Status.MarkSeedSecretFailed(v1alpha1.ReasonInvalidSeedSecret, "seed secret does not contain seed data, delete the secret for a new keypair")

		return false, nil
	}

	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		acc.Status.MarkSeedSecretFailed(v1alpha1.ReasonInvalidSeedSecret, "failed to parse seed: %s", err.Error())

		return false, nil
	}

	want, err := resources.NewKeyPairSecretBuilderFromSecret(got, r.Scheme).Build(acc, kp)
	if err != nil {
		logger.Error(err, "failed to build desired keypair secret")

		acc.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return false, err
	}

	if !equality.Semantic.DeepEqual(got, want) {
		logger.V(1).Info("seed secret does not match desired state, updating")

		err = r.Client.Update(ctx, want)
		if err != nil {
			logger.Error(err, "failed to update seed secret")

			acc.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

			return false, err
		}

		r.EventRecorder.Eventf(acc, v1.EventTypeNormal, "SeedSecretUpdated", "updated secret: %s/%s", want.Namespace, want.Name)
	}

	pubkey, _ := kp.PublicKey()

	acc.Status.MarkSeedSecretReady(pubkey, want.Name)

	return true, nil
}

func (r *AccountReconciler) ensureSigningKeysUpdated(ctx context.Context, acc *v1alpha1.Account) error {
	// using metav1.LabelSelectorAsSelector() returns labels.Nothing() if the label selector is nil, which is not what
	// we want, so default to labels.Everything()
	labelSelector := labels.Everything()

	if acc.Spec.SigningKeysSelector != nil {
		var err error
		labelSelector, err = metav1.LabelSelectorAsSelector(acc.Spec.SigningKeysSelector)
		if err != nil {
			// TODO: this should be part of a ValidatingAdmissionWebhook and reject if the label selector is invalid
			r.EventRecorder.Eventf(acc, v1.EventTypeWarning, "InvalidSigningKeysSelector", "Failed to parse label selector: %s", err.Error())

			return err
		}
	}

	var listOptions metav1.ListOptions

	if !labelSelector.Empty() {
		listOptions.LabelSelector = labelSelector.String()
	}

	skList, err := r.AccountsV1Alpha1.SigningKeys(acc.Namespace).List(ctx, listOptions)
	if err != nil {
		// this should be a temporary error on the api-server so don't record and event and hope it goes away
		return fmt.Errorf("failed to list SigningKeys: %w", err)
	}

	nextSKs := helpers.NextSigningKeys(acc.UID, acc.Status.SigningKeys, skList)

	if !equality.Semantic.DeepEqual(acc.Status.SigningKeys, nextSKs) {
		r.EventRecorder.Event(acc, v1.EventTypeNormal, "SigningKeysChanged", "")
	}

	acc.Status.MarkSigningKeysUpdated(nextSKs)

	return nil
}

func (r *AccountReconciler) reconcileJWTSecret(ctx context.Context, acc *v1alpha1.Account, issuerKP nkeys.KeyPair) (ajwt string, ok bool, err error) {
	logger := log.FromContext(ctx)

	// we want to check that any existing secret decodes to match wantClaims, if it doesn't then we will use nextJWT
	// to create/update the secret. We cannot just compare the JWTs from the secret and accountJWT because the JWTs are
	// timestamped with the `iat` claim so will never match.
	wantClaims, nextJWT, err := nsc.CreateAccountClaims(acc, issuerKP)
	if err != nil {
		acc.Status.MarkJWTSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return "", false, err
	}

	got, err := r.CoreV1.Secrets(acc.Namespace).Get(ctx, acc.Spec.JWTSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("JWT secret not found, creating new secret")

			return nextJWT, true, r.createJWTSecret(ctx, acc, nextJWT)
		}

		logger.Error(err, "failed to get JWT secret")

		acc.Status.MarkJWTSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return "", false, err
	}

	return r.ensureJWTSecretUpToDate(ctx, acc, wantClaims, got, nextJWT)
}

func (r *AccountReconciler) loadIssuerSeed(ctx context.Context, acc *v1alpha1.Account, issuer v1alpha1.KeyPairable) (nkeys.KeyPair, bool, error) {
	logger := log.FromContext(ctx)

	keyPair := issuer.GetKeyPair()
	if keyPair == nil {
		logger.Info("WARNING! issuer KeyPair is nil, but condition checks should have caught this")

		acc.Status.MarkIssuerResolveFailed(v1alpha1.ReasonUnknownError, "issuer KeyPair is nil")

		return nil, false, nil
	}

	skSeedSecret, err := r.CoreV1.Secrets(issuer.GetNamespace()).Get(ctx, keyPair.SeedSecretName, metav1.GetOptions{})
	if err != nil {
		logger.V(1).Info("failed to get issuer seed", "issuer", issuer.GetName())

		acc.Status.MarkIssuerResolveUnknown(v1alpha1.ReasonIssuerSeedError, "failed to get issuer seed: %s", err.Error())

		if errors.IsNotFound(err) {
			// this will be enqueued again when the secret is created and the issuer's status is updated
			return nil, false, nil
		}

		return nil, false, err
	}

	seed, ok := skSeedSecret.Data[v1alpha1.NatsSecretSeedKey]
	if !ok {
		acc.Status.MarkIssuerResolveFailed(v1alpha1.ReasonMalformedSeedSecret, "secret missing required field: %s", v1alpha1.NatsSecretSeedKey)

		return nil, false, nil
	}

	prefix, _, err := nkeys.DecodeSeed(seed)
	if err != nil {
		acc.Status.MarkIssuerResolveFailed(v1alpha1.ReasonMalformedSeedSecret, "failed to parse seed: %s", err.Error())

		return nil, false, nil
	}

	if prefix != nkeys.PrefixByteOperator {
		acc.Status.MarkIssuerResolveFailed(
			v1alpha1.ReasonMalformedSeedSecret,
			"unexpected seed prefix, wanted %q but got %q",
			nkeys.PrefixByteOperator.String(),
			prefix.String(),
		)

		return nil, false, nil
	}

	// we've already decoded the seed once to check the prefix, so we can ignore this error
	kp, _ := nkeys.FromSeed(seed)

	pk, err := kp.PublicKey()
	if err != nil {
		logger.Error(err, "failed to get public key from seed")

		acc.Status.MarkIssuerResolveFailed(v1alpha1.ReasonUnknownError, "failed to get public key from seed: %s", err.Error())

		return nil, false, nil
	}

	// check that the public key generated from the secret matches the public key in the issuer's KeyPair status, if
	// this fails then the issuer is probably going to reconcile again soon, and we'll be enqueued again afterwards.
	if pk != keyPair.PublicKey {
		acc.Status.MarkIssuerResolveFailed(
			v1alpha1.ReasonPublicKeyMismatch,
			"public key mismatch, wanted %q but got %q",
			keyPair.PublicKey,
			pk,
		)

		return nil, false, nil
	}

	acc.Status.MarkIssuerResolved()

	return kp, true, nil
}

func (r *AccountReconciler) createJWTSecret(ctx context.Context, acc *v1alpha1.Account, accountJWT string) error {
	logger := log.FromContext(ctx)

	secret, err := resources.NewJWTSecretBuilder(r.Scheme).Build(acc, accountJWT)
	if err != nil {
		logger.Error(err, "failed to build account keypair secret")

		acc.Status.MarkJWTSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	if err := controllerutil.SetControllerReference(acc, secret, r.Scheme); err != nil {
		logger.Error(err, "failed to set controller reference on account JWT secret")

		acc.Status.MarkJWTSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	if err := r.Client.Create(ctx, secret); err != nil {
		logger.Error(err, "failed to create account JWT secret")

		acc.Status.MarkJWTSecretFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	r.EventRecorder.Eventf(acc, v1.EventTypeNormal, "JWTSecretCreated", "created secret: %s/%s", secret.Namespace, secret.Name)

	return nil
}

// ensureJWTSecretUpToDate compares that the existing JWT secret decodes and matches the expected claims, if it does not
// match the secret will be updated with the nextJWT value.
func (r *AccountReconciler) ensureJWTSecretUpToDate(ctx context.Context, acc *v1alpha1.Account, wantClaims *jwt.AccountClaims, got *v1.Secret, nextJWT string) (string, bool, error) {
	logger := log.FromContext(ctx)

	gotJWT, ok := got.Data[v1alpha1.NatsSecretJWTKey]
	if !ok {
		// TODO: should we be checking owner references here? If we own it then we should be okay to delete it, but if
		//  not we tell the user to delete it manually, and they'll either do so, or update the spec to use a new name.
		acc.Status.MarkJWTSecretFailed(v1alpha1.ReasonInvalidJWTSecret, "JWT secret does not contain JWT data, delete the secret to generate a new JWT")

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

		acc.Status.MarkJWTSecretReady()

		return string(gotJWT), true, nil
	}

	want, err := resources.NewJWTSecretBuilderFromSecret(got, r.Scheme).Build(acc, nextJWT)
	if err != nil {
		logger.Error(err, "failed to build desired JWT secret")

		acc.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return "", false, err
	}

	err = r.Client.Update(ctx, want)
	if err != nil {
		logger.Error(err, "failed to update JWT secret")

		acc.Status.MarkSeedSecretUnknown(v1alpha1.ReasonUnknownError, err.Error())

		return "", false, err
	}

	r.EventRecorder.Eventf(acc, v1.EventTypeNormal, "SeedSecretUpdated", "updated secret: %s/%s", want.Namespace, want.Name)

	acc.Status.MarkJWTSecretReady()

	return nextJWT, true, nil
}

func (r *AccountReconciler) ensureJWTPushed(ctx context.Context, acc *v1alpha1.Account, operator *v1alpha1.Operator, issuer nkeys.KeyPair, ajwt string) error {
	logger := log.FromContext(ctx)

	sysSeed, err := r.SysAccountLoader.Load(ctx, operator)
	if err != nil {
		logger.Error(err, "failed to load system account")

		acc.Status.MarkJWTPushFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	opts, err := r.getNATSOptions(ctx, operator)
	if err != nil {
		logger.Error(err, "failed to get NATS options")

		acc.Status.MarkJWTPushFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	nscClient, err := nsc.Connect(operator.Spec.AccountServerURL, issuer, sysSeed, opts...)
	if err != nil {
		logger.Error(err, "failed to connect to account server")

		acc.Status.MarkJWTPushFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	defer nscClient.Close()

	if err = nscClient.Push(ctx, ajwt); err != nil {
		logger.Error(err, "failed to push account JWT to account server")

		acc.Status.MarkJWTPushFailed(v1alpha1.ReasonJWTPushError, err.Error())

		return err
	}

	acc.Status.MarkJWTPushed()

	return nil
}

func (r *AccountReconciler) finalizeAccount(ctx context.Context, acc *v1alpha1.Account) error {
	logger := log.FromContext(ctx)

	if !acc.Status.GetCondition(v1alpha1.AccountConditionJWTSecretReady).IsTrue() {
		logger.Info("JWT secret is not ready, skipping finalization")

		return nil
	}

	if !acc.Status.GetCondition(v1alpha1.AccountConditionJWTPushed).IsTrue() {
		logger.Info("JWT not pushed, skipping finalization")

		return nil
	}

	// TODO: anything from here on out should follow the happy path. If not, then someone is deleting things en-masse
	//  and isn't letting our dear old operator clean up after itself.
	//  Can we remove the deletion timestamp and add an Event to the resource to indicate that it was attempted to be
	//  deleted, but failed? This could let the user wait for everything to reconcile again, then a deletion would work.
	//  If the user really wanted to circumvent this, they could force delete and remove the finalizer - at which point
	//  "if the user wants to do that, then it's their *** fault".

	operatorRef := acc.Status.OperatorRef
	operator, err := r.AccountsV1Alpha1.Operators(operatorRef.Namespace).Get(ctx, operatorRef.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("operator not found, skipping finalization")

			return nil
		}

		logger.Error(err, "failed to get operator during finalization")

		return fmt.Errorf("operator could not be loaded: %w", err)
	}

	if operator.Status.KeyPair == nil {
		return fmt.Errorf("operator not ready")
	}

	operatorSeed, err := r.CoreV1.Secrets(operator.Namespace).Get(ctx, operator.Status.KeyPair.SeedSecretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("unable to load operator seed: %w", err)
	}

	operatorSeedData, ok := operatorSeed.Data[v1alpha1.NatsSecretSeedKey]
	if !ok {
		return fmt.Errorf("operator seed secret missing property, %q", v1alpha1.NatsSecretSeedKey)
	}

	operatorKP, err := nkeys.FromSeed(operatorSeedData)
	if err != nil {
		return fmt.Errorf("failed to parse operator seed data: %w", err)
	}

	sysSeed, err := r.SysAccountLoader.Load(ctx, operator)
	if err != nil {
		// not sure what errors should allow finalization to skip vs fail for a retry, for now we'll only skip if the
		// system account doesn't exist, otherwise we'll fail for a retry
		if errors.IsNotFound(err) {
			logger.Info("system account not found, skipping finalization")

			return nil
		}

		logger.Error(err, "failed to load system account during finalization")

		return err
	}

	opts, err := r.getNATSOptions(ctx, operator)
	if err != nil {
		logger.Error(err, "failed to get NATS options")

		acc.Status.MarkJWTPushFailed(v1alpha1.ReasonUnknownError, err.Error())

		return err
	}

	nscClient, err := nsc.Connect(operator.Spec.AccountServerURL, operatorKP, sysSeed, opts...)
	if err != nil {
		logger.Error(err, "failed to connect to account server during finalization")

		return err
	}

	defer nscClient.Close()

	if err = nscClient.Delete(ctx, acc.Status.KeyPair.PublicKey); err != nil {
		logger.Error(err, "failed to delete account JWT")

		return err
	}

	return nil
}

func (r *AccountReconciler) getNATSOptions(ctx context.Context, operator *v1alpha1.Operator) ([]nats.Option, error) {
	if operator.Spec.TLSConfig == nil {
		return nil, nil
	}

	tlsConfig := operator.Spec.TLSConfig

	switch {
	case tlsConfig.CAFile != nil:
		caFile, err := r.loadCAFile(ctx, operator.Namespace, *tlsConfig.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load CA file: %w", err)
		}

		return []nats.Option{nsc.CABundle(caFile)}, nil
	default:
		return nil, fmt.Errorf("invalid TLS config: missing CA file")
	}
}

func (r *AccountReconciler) loadCAFile(ctx context.Context, ns string, selector v1.SecretKeySelector) ([]byte, error) {
	secret, err := r.CoreV1.Secrets(ns).Get(ctx, selector.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get caFile secret: %w", err)
	}

	key := "ca.crt"
	if selector.Key != "" {
		key = selector.Key
	}

	caFile, ok := secret.Data[key]
	if !ok {
		return nil, fmt.Errorf("caFile secret missing key %q", key)
	}

	return caFile, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.EventRecorder = mgr.GetEventRecorderFor("account-controller")

	logger := mgr.GetLogger().WithName("AccountReconciler")
	err := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Account{}).
		Owns(&v1.Secret{}).
		Watches(
			&source.Kind{Type: &v1alpha1.SigningKey{}},
			handler.EnqueueRequestsFromMapFunc(func(obj client.Object) []reconcile.Request {
				signingKey, ok := obj.(*v1alpha1.SigningKey)
				if !ok {
					logger.Info("SigningKey watcher received non-SigningKey object",
						"kind", obj.GetObjectKind().GroupVersionKind().String())
					return nil
				}

				ownerRef := signingKey.Status.OwnerRef
				if ownerRef == nil {
					return nil
				}

				accountGVK := v1alpha1.GroupVersion.WithKind("Account")
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
