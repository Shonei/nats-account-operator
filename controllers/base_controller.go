package controllers

import (
	"context"
	"fmt"
	"github.com/nats-io/nkeys"
	"github.com/versori-oss/nats-account-operator/api/accounts/v1alpha1"
	"github.com/versori-oss/nats-account-operator/controllers/resources"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type BaseReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	CoreV1        corev1.CoreV1Interface
	EventRecorder record.EventRecorder
}

func (r *BaseReconciler) ensureSeedSecretUpToDate(ctx context.Context, owner client.Object, got *v1.Secret) (nkeys.KeyPair, bool, error) {
	logger := log.FromContext(ctx)

	seed, ok := got.Data[v1alpha1.NatsSecretSeedKey]
	if !ok {
		return nil, true, ConditionFailed(v1alpha1.ReasonInvalidSeedSecret, "seed secret does not contain seed data, delete the secret for a new keypair")
	}

	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		return nil, true, ConditionFailed(v1alpha1.ReasonInvalidSeedSecret, "failed to parse seed: %s", err.Error())
	}

	want, err := resources.NewKeyPairSecretBuilderFromSecret(got, r.Scheme).Build(owner, kp)
	if err != nil {
		logger.Error(err, "failed to build desired keypair secret")

		return kp, true, ConditionFailed(v1alpha1.ReasonUnknownError, err.Error())
	}

	if !equality.Semantic.DeepEqual(got, want) {
		logger.V(1).Info("seed secret does not match desired state, updating")

		err = r.Client.Update(ctx, want)
		if err != nil {
			logger.Error(err, "failed to update seed secret")

			return kp, false, ConditionFailed(v1alpha1.ReasonUnknownError, err.Error())
		}

		r.EventRecorder.Eventf(owner, v1.EventTypeNormal, "SeedSecretUpdated", "updated secret: %s/%s", want.Namespace, want.Name)
	}

	return kp, true, nil
}

// resolveIssuer resolves the issuer reference to a KeyPairable object. This is abstracted to support issuers being
// either a SigningKey, or an Operator/Account where the object being reconciled is an Account/User respectively.
//
// The returned bool indicates whether the process has handled the reconciliation properly. If an error is returned,
// this should be for informational purposes (or to mark a status condition) and not returned back to the controller
// runtime. Usually this should only be false in combination with an error if the error is caused by a temporary or
// unknown issue such as network errors etc.
func (r *BaseReconciler) resolveIssuer(ctx context.Context, issuer v1alpha1.IssuerReference, fallbackNamespace string) (kp v1alpha1.KeyPairable, ok bool, err error) {
	logger := log.FromContext(ctx)

	issuerGVK := issuer.Ref.GetGroupVersionKind()

	obj, err := r.Scheme.New(issuerGVK)
	if err != nil {
		logger.Error(err, "failed to create issuer object from scheme", "issuer_gvk", issuerGVK.String())

		return nil, true, ConditionFailed(
			v1alpha1.ReasonUnsupportedIssuer, "unsupported GroupVersionKind: %s", err.Error())
	}

	issuerObj, ok := obj.(client.Object)
	if !ok {
		logger.Info("failed to convert runtime.Object to client.Object",
			"issuer_gvk", issuerGVK.String(),
			"obj_type", fmt.Sprintf("%T", obj),
		)

		return nil, true, ConditionFailed(
			v1alpha1.ReasonUnsupportedIssuer, "runtime.Object cannot be converted to client.Object", issuerGVK.String())
	}

	// .issuer.ref.namespace is optional, so default to the Account's namespace if not set
	if issuer.Ref.Namespace != "" {
		fallbackNamespace = issuer.Ref.Namespace
	}

	err = r.Get(ctx, client.ObjectKey{
		Namespace: fallbackNamespace,
		Name:      issuer.Ref.Name,
	}, issuerObj)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, true, ConditionFailed(
				v1alpha1.ReasonNotFound, "%s, %s/%s: not found", issuerGVK.String(), fallbackNamespace, issuer.Ref.Name)
		}

		return nil, false, ConditionUnknown(v1alpha1.ReasonUnknownError, "%s", err.Error())
	}

	keyPairable, ok := issuerObj.(v1alpha1.KeyPairable)
	if !ok {
		logger.Info("issuer does not implement KeyPairable interface", "issuer_type", fmt.Sprintf("%T", issuer))

		return nil, true, ConditionFailed(
			v1alpha1.ReasonUnsupportedIssuer, "issuer does not implement KeyPairable interface")
	}

	conditions := keyPairable.GetConditionSet().Manage(keyPairable.GetStatus())

	// Initialize the conditions if they are not already set, not doing this causes a nil-pointer dereference panic
	conditions.InitializeConditions()

	seedReadyCondition := conditions.GetCondition(v1alpha1.KeyPairableConditionSeedSecretReady)
	if !seedReadyCondition.IsTrue() {
		logger.V(1).Info("issuer seed secret is not ready", "issuer_type", fmt.Sprintf("%T", issuer), "reason", seedReadyCondition.Reason, "message", seedReadyCondition.Message)

		return nil, true, ConditionUnknown(
			v1alpha1.ReasonNotReady, "issuer seed secret is not ready")
	}

	return keyPairable, true, nil
}

func (r *BaseReconciler) resolveSigningKeyOwner(ctx context.Context, sk *v1alpha1.SigningKey) (client.Object, bool, error) {
	logger := log.FromContext(ctx)

	if !sk.Status.GetCondition(v1alpha1.SigningKeyConditionOwnerResolved).IsTrue() {
		return nil, true, ConditionUnknown(v1alpha1.ReasonNotReady, "signing key owner has not been resolved")
	}

	gvk := sk.Status.OwnerRef.GetGroupVersionKind()

	obj, err := r.Scheme.New(gvk)
	if err != nil {
		logger.Error(err, "failed to create owner object from scheme", "owner_gvk", gvk.String())

		return nil, true, ConditionFailed(
			v1alpha1.ReasonInvalidSigningKeyOwner, "unsupported GroupVersionKind: %s", err.Error())
	}

	owner, ok := obj.(client.Object)
	if !ok {
		logger.Info("failed to convert runtime.Object to client.Object",
			"owner_gvk", gvk.String(),
			"owner_type", fmt.Sprintf("%T", obj),
		)

		return nil, true, ConditionFailed(
			v1alpha1.ReasonInvalidSigningKeyOwner, "runtime.Object cannot be converted to client.Object", gvk.String())
	}

	err = r.Client.Get(ctx, client.ObjectKey{
		Namespace: sk.Status.OwnerRef.Namespace,
		Name:      sk.Status.OwnerRef.Name,
	}, owner)
	if err != nil {
		logger.Error(err, "failed to get signing key owner")

		if errors.IsNotFound(err) {
			return nil, true, ConditionFailed(v1alpha1.ReasonNotFound, err.Error())
		}

		return nil, false, ConditionUnknown(v1alpha1.ReasonUnknownError, err.Error())
	}

	return owner, true, nil
}

func (r *BaseReconciler) loadIssuerSeed(ctx context.Context, issuer v1alpha1.KeyPairable, wantPrefix nkeys.PrefixByte) (nkeys.KeyPair, bool, error) {
	logger := log.FromContext(ctx)

	keyPair := issuer.GetKeyPair()
	if keyPair == nil {
		logger.Info("WARNING! issuer KeyPair is nil, but condition checks should have caught this")

		return nil, true, ConditionFailed(v1alpha1.ReasonUnknownError, "issuer KeyPair is nil")
	}

	skSeedSecret, err := r.CoreV1.Secrets(issuer.GetNamespace()).Get(ctx, keyPair.SeedSecretName, metav1.GetOptions{})
	if err != nil {
		logger.V(1).Info("failed to get issuer seed", "issuer", issuer.GetName())

		// if err is NotFound, there's no need to requeue since creation of the secret should trigger new
		// reconciliations
		return nil, errors.IsNotFound(err), ConditionUnknown(v1alpha1.ReasonIssuerSeedError, "failed to get issuer seed: %s", err.Error())
	}

	seed, ok := skSeedSecret.Data[v1alpha1.NatsSecretSeedKey]
	if !ok {
		return nil, true, ConditionFailed(v1alpha1.ReasonMalformedSeedSecret, "secret missing required field: %s", v1alpha1.NatsSecretSeedKey)
	}

	prefix, _, err := nkeys.DecodeSeed(seed)
	if err != nil {
		return nil, true, ConditionFailed(v1alpha1.ReasonMalformedSeedSecret, "failed to parse seed: %s", err.Error())
	}

	if prefix != wantPrefix {
		return nil, true, ConditionFailed(
			v1alpha1.ReasonMalformedSeedSecret,
			"unexpected seed prefix, wanted %q but got %q",
			wantPrefix.String(),
			prefix.String(),
		)
	}

	// we've already decoded the seed once to check the prefix, so we can ignore this error
	kp, _ := nkeys.FromSeed(seed)

	pk, err := kp.PublicKey()
	if err != nil {
		logger.Error(err, "failed to get public key from seed")

		return nil, true, ConditionFailed(v1alpha1.ReasonUnknownError, "failed to get public key from seed: %s", err.Error())
	}

	// check that the public key generated from the secret matches the public key in the issuer's KeyPair status, if
	// this fails then the issuer is probably going to reconcile again soon, and we'll be enqueued again afterwards.
	if pk != keyPair.PublicKey {
		return nil, false, ConditionFailed(
			v1alpha1.ReasonPublicKeyMismatch,
			"public key mismatch, wanted %q but got %q",
			keyPair.PublicKey,
			pk,
		)
	}

	return kp, true, nil
}
