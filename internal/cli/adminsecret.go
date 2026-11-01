package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The director's admin API bearer secret. setup creates it once; it is never
// rotated or printed. The director reads it from $BP_ADMIN_TOKEN, the CLI
// and board read it with the user's own kube credentials.
const (
	adminSecret    = "bp-director-admin"
	adminSecretKey = "token"
)

// ensureAdminSecret creates the admin Secret if it doesn't exist. An existing
// one is left exactly as it is.
func ensureAdminSecret(ctx context.Context, kube kubernetes.Interface, ns string) (created bool, err error) {
	_, err = kube.CoreV1().Secrets(ns).Get(ctx, adminSecret, metav1.GetOptions{})
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get secret %s: %w", adminSecret, err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return false, err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: adminSecret, Namespace: ns,
			Labels: map[string]string{managedByLabel: managedByValue, "app.kubernetes.io/name": directorDeploy},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{adminSecretKey: []byte(hex.EncodeToString(b))},
	}
	_, err = kube.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) {
		return false, nil // created concurrently; keep theirs
	}
	if err != nil {
		return false, fmt.Errorf("create secret %s: %w", adminSecret, err)
	}
	return true, nil
}

// readAdminToken reads the admin bearer secret.
func readAdminToken(ctx context.Context, kube kubernetes.Interface, ns string) (string, error) {
	s, err := kube.CoreV1().Secrets(ns).Get(ctx, adminSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("secret %s is missing in %s: run `boardingparty setup -n %s`", adminSecret, ns, ns)
	}
	if err != nil {
		return "", fmt.Errorf("read secret %s (the director's admin key): %w", adminSecret, err)
	}
	t := string(s.Data[adminSecretKey])
	if t == "" {
		return "", fmt.Errorf("secret %s has no %q key: delete it and run `boardingparty setup -n %s`", adminSecret, adminSecretKey, ns)
	}
	return t, nil
}
