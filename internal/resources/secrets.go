/*
Copyright 2026.

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

package resources

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

const (
	// PasswordKey is the data key of the generated admin and Redis password Secrets.
	PasswordKey = "password"
	// AdminPasswordLength / RedisPasswordLength are the generated password lengths
	// (spec 003 research R1, R2).
	AdminPasswordLength = 24
	RedisPasswordLength = 32
)

// AdminSecretName is the Secret holding a Shop's generated initial admin password (003 R1).
func AdminSecretName(shop *shophubv1alpha1.Shop) string { return shop.Name + "-admin" }

// RedisAuthSecretName is the first Secret holding a light Shop's generated Redis password
// (003 R2). Later generations add a suffix (RedisAuthSecretGeneration).
func RedisAuthSecretName(shop *shophubv1alpha1.Shop) string { return shop.Name + "-redis-auth" }

// RedisAuthSecretGeneration returns the generation of a Redis password Secret name: 1 for
// RedisAuthSecretName, n for "<that>-n" (n ≥ 2), and 0 for a name that is not the Shop's.
func RedisAuthSecretGeneration(shop *shophubv1alpha1.Shop, name string) int {
	base := RedisAuthSecretName(shop)
	if name == base {
		return 1
	}
	suffix, ok := strings.CutPrefix(name, base+"-")
	if !ok || suffix == "" || suffix[0] == '0' {
		return 0
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 2 {
		return 0
	}
	return n
}

// NextRedisAuthSecretName is the Secret name that replaces a lost Redis password Secret
// (operator 0.2.1): giving the Redis a new Secret name makes the Redis operator roll its pod
// with the new password, while the data on the PVC stays.
func NextRedisAuthSecretName(shop *shophubv1alpha1.Shop, current string) string {
	n := max(RedisAuthSecretGeneration(shop, current), 1) + 1
	return RedisAuthSecretName(shop) + "-" + strconv.Itoa(n)
}

// RandomPassword returns n URL-safe characters from crypto/rand (6 bits each).
func RandomPassword(n int) (string, error) {
	buf := make([]byte, (n*6+7)/8+1)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)[:n], nil
}

// PasswordSecret builds an Opaque Secret with one generated password under PasswordKey.
// The caller sets the owner reference and creates it once; it is never updated (003 contract).
func PasswordSecret(shop *shophubv1alpha1.Shop, name string, labels map[string]string, password string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: shop.Namespace, Labels: merge(labels),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{PasswordKey: password},
	}
}
