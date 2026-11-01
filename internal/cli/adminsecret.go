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

// ensureAdminSecret returns the admin Secret, creating it if it doesn't
// exist. An existing one is left exactly as it is.
func ensureAdminSecret(ctx context.Context, kube kubernetes.Interface, ns string) (*corev1.Secret, bool, error) {
	cur, err := kube.CoreV1().Secrets(ns).Get(ctx, adminSecret, metav1.GetOptions{})
	if err == nil {
		return cur, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("get secret %s: %w", adminSecret, err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, false, err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: adminSecret, Namespace: ns,
			Labels: map[string]string{managedByLabel: managedByValue, "app.kubernetes.io/name": directorDeploy},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{adminSecretKey: []byte(hex.EncodeToString(b))},
	}
	made, err := kube.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) {
		// Created concurrently; keep theirs.
		cur, err = kube.CoreV1().Secrets(ns).Get(ctx, adminSecret, metav1.GetOptions{})
		if err != nil {
			return nil, false, fmt.Errorf("get secret %s: %w", adminSecret, err)
		}
		return cur, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("create secret %s: %w", adminSecret, err)
	}
	return made, true, nil
}

// adminTokenOf returns the token in the admin Secret.
func adminTokenOf(s *corev1.Secret, ns string) (string, error) {
	t := string(s.Data[adminSecretKey])
	if t == "" {
		return "", fmt.Errorf("secret %s has no %q key: delete it and run `boardingparty setup -n %s`", adminSecret, adminSecretKey, ns)
	}
	return t, nil
}

// adminSecretAnnotation on the director's pod template carries the admin
// Secret's UID (not its value), so recreating the Secret rolls the pods and
// they pick up the new key.
const adminSecretAnnotation = "boardingparty.io/admin-secret-uid"

// readAdminToken reads the admin bearer secret.
func readAdminToken(ctx context.Context, kube kubernetes.Interface, ns string) (string, error) {
	s, err := kube.CoreV1().Secrets(ns).Get(ctx, adminSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("secret %s is missing in %s: run `boardingparty setup -n %s`", adminSecret, ns, ns)
	}
	if err != nil {
		return "", fmt.Errorf("read secret %s (the director's admin key): %w", adminSecret, err)
	}
	return adminTokenOf(s, ns)
}
