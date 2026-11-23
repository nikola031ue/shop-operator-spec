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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/discord"
)

// fakeDiscord is an in-memory Discord used by the DiscordChannel envtest cases.
type fakeDiscord struct {
	mu       sync.Mutex
	channels map[string]discord.Channel
	webhooks map[string]int // channel ID -> webhooks created
	nextID   int
	creates  int
	err      error // returned by every call while set
	tokens   []string
}

func newFakeDiscord() *fakeDiscord {
	return &fakeDiscord{channels: map[string]discord.Channel{}, webhooks: map[string]int{}}
}

func (f *fakeDiscord) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *fakeDiscord) ListChannels(context.Context, string) ([]discord.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]discord.Channel, 0, len(f.channels))
	for _, c := range f.channels {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeDiscord) CreateChannel(_ context.Context, _, name, topic string) (discord.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return discord.Channel{}, f.err
	}
	f.nextID++
	f.creates++
	c := discord.Channel{ID: fmt.Sprint(1000 + f.nextID), Name: name, Topic: topic}
	f.channels[c.ID] = c
	return c, nil
}

func (f *fakeDiscord) CreateWebhook(_ context.Context, channelID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if _, ok := f.channels[channelID]; !ok {
		return "", fmt.Errorf("unknown channel %s", channelID)
	}
	f.webhooks[channelID]++
	return fmt.Sprintf("https://discord.test/api/webhooks/%s/hook-secret-%d", channelID, f.webhooks[channelID]), nil
}

func (f *fakeDiscord) DeleteChannel(_ context.Context, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.channels, channelID)
	return nil
}

func (f *fakeDiscord) snapshot() (channels, creates int, webhooks map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.channels), f.creates, maps.Clone(f.webhooks)
}

const testToken = "bot-token-must-never-leak"

// startDiscordManager runs a DiscordChannel reconciler limited to ns, talking to fake.
func startDiscordManager(ns string, fake *fakeDiscord, guildID string) {
	GinkgoHelper()
	dcConfig := testConfig
	dcConfig.DiscordGuildID = guildID
	dcConfig.DiscordTokenSecret = types.NamespacedName{Namespace: ns, Name: "discord-token"}
	opts := CacheOptions()
	opts.DefaultNamespaces = map[string]cache.Config{ns: {}}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0", Cache: opts,
		// Each test starts its own "discordchannel" controller in the same process.
		Controller: ctrlconfig.Controller{SkipNameValidation: new(true)},
	})
	Expect(err).NotTo(HaveOccurred())
	mctx, mcancel := context.WithCancel(ctx)
	DeferCleanup(mcancel)
	Expect(SetupIndexes(mctx, mgr)).To(Succeed())
	Expect((&DiscordChannelReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(),
		Config: dcConfig,
		NewClient: func(token string) discord.Client {
			fake.mu.Lock()
			fake.tokens = append(fake.tokens, token)
			fake.mu.Unlock()
			return fake
		},
		CredentialsRetry: 500 * time.Millisecond,
	}).SetupWithManager(mgr)).To(Succeed())
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(mctx)).To(Succeed())
	}()
}

// createTokenSecret creates the hand-made, unlabelled bot token Secret (research R15).
func createTokenSecret(ns string) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "discord-token", Namespace: ns},
		Data:       map[string][]byte{DiscordTokenKey: []byte(testToken)},
	})).To(Succeed())
}

func getChannel(g Gomega, ns, name string) *shophubv1alpha1.DiscordChannel {
	var d shophubv1alpha1.DiscordChannel
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d)).To(Succeed())
	return &d
}

func getWebhookSecret(g Gomega, ns, name string) *corev1.Secret {
	var s corev1.Secret
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name + "-discord-webhook"}, &s)).To(Succeed())
	return &s
}

var _ = Describe("DiscordChannel reconciler (US3, FR-012–FR-016)", func() {
	var ns string
	var fake *fakeDiscord

	BeforeEach(func() {
		ns = newNamespace()
		fake = newFakeDiscord()
	})

	readyChannel := func(name string) *shophubv1alpha1.DiscordChannel {
		GinkgoHelper()
		d := newDiscordChannel(ns, name)
		d.Labels = map[string]string{LabelOwner: testOwner}
		Expect(k8sClient.Create(ctx, d)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getChannel(g, ns, name).Status.Conditions)).To(Equal("True/Ready"))
		}, reasonTimeout).Should(Succeed())
		return getChannel(Default, ns, name)
	}

	Context("with credentials", func() {
		BeforeEach(func() {
			createTokenSecret(ns)
			startDiscordManager(ns, fake, "g1")
		})

		It("scenario 1: creates the channel once and stores the webhook in an owned Secret", func() {
			d := readyChannel("acme-alerts")
			Expect(d.Status.ChannelID).NotTo(BeEmpty())
			Expect(d.Status.WebhookSecretName).To(Equal("acme-alerts-discord-webhook"))
			Expect(d.Finalizers).To(ContainElement(FinalizerDiscordCleanup))

			chans, creates, _ := fake.snapshot()
			Expect(chans).To(Equal(1))
			Expect(creates).To(Equal(1))
			ch := fake.channels[d.Status.ChannelID]
			Expect(ch.Name).To(Equal("acme-alerts"))
			Expect(ch.Topic).To(ContainSubstring("shophub-uid:" + string(d.UID)))

			sec := getWebhookSecret(Default, ns, "acme-alerts")
			url := string(sec.Data[WebhookSecretKey])
			Expect(url).To(HavePrefix("https://discord.test/api/webhooks/" + d.Status.ChannelID + "/"))
			Expect(sec.OwnerReferences).To(HaveLen(1))
			Expect(sec.OwnerReferences[0].UID).To(Equal(d.UID))
			Expect(sec.Labels).To(HaveKeyWithValue(LabelManagedBy, ManagedByValue))
			Expect(sec.Labels).To(HaveKeyWithValue(LabelOwner, testOwner))

			By("finding the hand-created, unlabelled token Secret through the API reader")
			fake.mu.Lock()
			Expect(fake.tokens).To(ContainElement(testToken))
			fake.mu.Unlock()

			By("never exposing the webhook URL or token in status or logs (SC-007)")
			raw, err := json.Marshal(d)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).NotTo(ContainSubstring(url))
			Expect(logBuf.String()).NotTo(ContainSubstring(url))
			Expect(logBuf.String()).NotTo(ContainSubstring(testToken))
		})

		It("adopts its channel by topic after losing status, without a second channel", func() {
			d := readyChannel("adopt")
			id := d.Status.ChannelID
			Eventually(func(g Gomega) { // wipe channelID as if status were lost
				cur := getChannel(g, ns, "adopt")
				cur.Status.ChannelID = ""
				g.Expect(k8sClient.Status().Update(ctx, cur)).To(Succeed())
			}, reasonTimeout).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(getChannel(g, ns, "adopt").Status.ChannelID).To(Equal(id))
			}, reasonTimeout).Should(Succeed())
			_, creates, _ := fake.snapshot()
			Expect(creates).To(Equal(1))
		})

		It("restores a hand-deleted webhook Secret with a new webhook on the same channel (G4)", func() {
			d := readyChannel("heal")
			Expect(k8sClient.Delete(ctx, getWebhookSecret(Default, ns, "heal"))).To(Succeed())
			Eventually(func(g Gomega) {
				sec := getWebhookSecret(g, ns, "heal")
				g.Expect(string(sec.Data[WebhookSecretKey])).To(HaveSuffix("/hook-secret-2"))
			}, reasonTimeout).Should(Succeed())
			chans, creates, hooks := fake.snapshot()
			Expect(chans).To(Equal(1))
			Expect(creates).To(Equal(1), "no second channel")
			Expect(hooks[d.Status.ChannelID]).To(Equal(2))
		})

		It("scenario 2: deleting removes the Discord channel and the resource", func() {
			d := readyChannel("bye")
			Expect(k8sClient.Delete(ctx, d)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, keyOf(d), &shophubv1alpha1.DiscordChannel{}))
			}, reasonTimeout).Should(BeTrue())
			chans, _, _ := fake.snapshot()
			Expect(chans).To(BeZero())
		})

		It("scenario 3: reports DiscordUnavailable on rate limits and recovers", func() {
			fake.setErr(&discord.RetryableError{Op: "list channels", Status: 429, RetryAfter: 200 * time.Millisecond})
			d := newDiscordChannel(ns, "busy")
			Expect(k8sClient.Create(ctx, d)).To(Succeed())
			Eventually(func(g Gomega) {
				c := getChannel(g, ns, "busy")
				g.Expect(readyOf(c.Status.Conditions)).To(Equal("False/" + ReasonDiscordUnavailable))
			}, reasonTimeout).Should(Succeed())
			fake.setErr(nil)
			Eventually(func(g Gomega) {
				g.Expect(readyOf(getChannel(g, ns, "busy").Status.Conditions)).To(Equal("True/Ready"))
			}, reasonTimeout).Should(Succeed())
		})

		It("reports DiscordForbidden on 401/403", func() {
			fake.setErr(&discord.ForbiddenError{Op: "list channels", Status: 403})
			Expect(k8sClient.Create(ctx, newDiscordChannel(ns, "nope"))).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(readyOf(getChannel(g, ns, "nope").Status.Conditions)).To(Equal("False/" + ReasonDiscordForbidden))
			}, reasonTimeout).Should(Succeed())
		})

		It("blocks deletion while a Shop references it, then cleans up", func() {
			d := readyChannel("ch")
			Expect(k8sClient.Create(ctx, newShop(ns, "s1", "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(getChannel(g, ns, "ch").Status.UsedBy).To(Equal([]string{"s1"}))
			}, reasonTimeout).Should(Succeed())

			Expect(k8sClient.Delete(ctx, d)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(readyOf(getChannel(g, ns, "ch").Status.Conditions)).To(Equal("False/" + ReasonInUse))
			}, reasonTimeout).Should(Succeed())
			chans, _, _ := fake.snapshot()
			Expect(chans).To(Equal(1), "channel kept while in use")

			Expect(k8sClient.Delete(ctx, &shophubv1alpha1.Shop{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: ns}})).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, keyOf(d), &shophubv1alpha1.DiscordChannel{}))
			}, reasonTimeout).Should(BeTrue())
			chans, _, _ = fake.snapshot()
			Expect(chans).To(BeZero())
		})

		It("scenario 4: the referencing Shop reports the channel's webhook Secret", func() {
			readyChannel("ch")
			Expect(k8sClient.Create(ctx, newWallet(ns, "w", addrA))).To(Succeed())
			Expect(k8sClient.Create(ctx, newShop(ns, "s4", "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())
			Eventually(func(g Gomega) {
				s := getShop(g, ns, "s4")
				g.Expect(s.Status.WebhookSecretName).To(Equal("ch-discord-webhook"))
				g.Expect(readyOf(s.Status.Conditions)).NotTo(Equal("False/" + ReasonDiscordChannelNotReady))
			}, reasonTimeout).Should(Succeed())
		})
	})

	It("reports DiscordCredentialsMissing without a guild ID", func() {
		createTokenSecret(ns)
		startDiscordManager(ns, fake, "")
		Expect(k8sClient.Create(ctx, newDiscordChannel(ns, "noguild"))).To(Succeed())
		Eventually(func(g Gomega) {
			c := getChannel(g, ns, "noguild")
			g.Expect(readyOf(c.Status.Conditions)).To(Equal("False/" + ReasonDiscordCredentialsMissing))
		}, reasonTimeout).Should(Succeed())
	})

	It("reports DiscordCredentialsMissing naming the token Secret, then recovers once it exists", func() {
		startDiscordManager(ns, fake, "g1")
		Expect(k8sClient.Create(ctx, newDiscordChannel(ns, "notoken"))).To(Succeed())
		Eventually(func(g Gomega) {
			c := getChannel(g, ns, "notoken")
			g.Expect(readyOf(c.Status.Conditions)).To(Equal("False/" + ReasonDiscordCredentialsMissing))
			msg := c.Status.Conditions[0].Message
			g.Expect(msg).To(ContainSubstring(ns + "/discord-token"))
		}, reasonTimeout).Should(Succeed())

		createTokenSecret(ns) // not watched: picked up by the periodic credentials retry
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getChannel(g, ns, "notoken").Status.Conditions)).To(Equal("True/Ready"))
		}, reasonTimeout).Should(Succeed())
		Expect(strings.Contains(logBuf.String(), testToken)).To(BeFalse())
	})
})
