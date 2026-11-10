package kubernetes_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/streetsmartops/incident-management-chatbot/core"
	kp "github.com/streetsmartops/incident-management-chatbot/providers/kubernetes"
)

func i32(n int32) *int32 { return &n }

func objects() []runtime.Object {
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}
	tmpl := func(img string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api", "pod-template-hash": "h"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: img}}},
		}
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "checkout", UID: "dep-uid", Annotations: map[string]string{"deployment.kubernetes.io/revision": "7"}},
		Spec:       appsv1.DeploymentSpec{Replicas: i32(3), Selector: sel, Template: tmpl("registry/api:v2.3.1")},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 3},
	}
	rs := func(rev, img string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "api-" + rev, Namespace: "checkout", Labels: map[string]string{"app": "api"},
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: "dep-uid"}}},
			Spec: appsv1.ReplicaSetSpec{Template: tmpl(img)},
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-7-abc", Namespace: "checkout", Labels: map[string]string{"app": "api"}}}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "checkout"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-7-abc"},
		Reason:         "BackOff", Message: "Back-off restarting failed container", Type: "Warning",
		LastTimestamp: metav1.NewTime(time.Date(2026, 11, 10, 2, 45, 0, 0, time.UTC)),
	}
	return []runtime.Object{dep, rs("7", "registry/api:v2.3.1"), rs("6", "registry/api:v2.3.0"), pod, ev}
}

func provider(t *testing.T) (*kp.Provider, *fake.Clientset) {
	cs := fake.NewSimpleClientset(objects()...)
	p := kp.New([]kp.Cluster{{Name: "prod-eu-1", Client: cs, Namespaces: []string{"checkout", "payments-*"}}})
	p.Now = func() time.Time { return time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC) }
	return p, cs
}

func TestNamespaceAllowList(t *testing.T) {
	if !kp.NamespaceAllowed([]string{"payments-*"}, "payments-eu") || kp.NamespaceAllowed([]string{"payments-*"}, "kube-system") || kp.NamespaceAllowed([]string{"*"}, "") {
		t.Fatal("glob matching")
	}
	p, _ := provider(t)
	var se *kp.ScopeError
	if _, err := p.ListWorkloads(context.Background(), "prod-eu-1", "kube-system"); !errors.As(err, &se) {
		t.Fatalf("namespace outside allow-list must be refused: %v", err)
	}
	if _, err := p.ListWorkloads(context.Background(), "unknown", "checkout"); !errors.As(err, &se) {
		t.Fatalf("unknown cluster must be refused: %v", err)
	}
}

func TestReads(t *testing.T) {
	p, _ := provider(t)
	ctx := context.Background()
	ws, err := p.ListWorkloads(ctx, "prod-eu-1", "checkout")
	if err != nil || len(ws) != 1 || ws[0].Desired != 3 || ws[0].Ready != 1 || ws[0].Revision != "7" {
		t.Fatalf("workloads: %+v %v", ws, err)
	}
	ref := core.WorkloadRef{Cluster: "prod-eu-1", Namespace: "checkout", Kind: "Deployment", Name: "api"}
	hist, err := p.RolloutHistory(ctx, ref)
	if err != nil || len(hist) != 2 || hist[0].Revision != "7" || !strings.Contains(hist[1].Images[0], "v2.3.0") {
		t.Fatalf("history: %+v %v", hist, err)
	}
	evs, err := p.WorkloadEvents(ctx, ref, time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC))
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Title, "BackOff") {
		t.Fatalf("events: %+v %v", evs, err)
	}
	logs, err := p.PodLogs(ctx, ref, time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC), 50)
	if err != nil || len(logs) == 0 {
		t.Fatalf("logs: %+v %v", logs, err)
	}
}

func TestDryRunIsReadOnlyAndExecuteDisabled(t *testing.T) {
	p, cs := provider(t)
	ctx := context.Background()
	base := map[string]string{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	before := len(cs.Actions())
	diff, err := p.DryRun(ctx, core.ActionSpec{Verb: kp.VerbRolloutUndo, Params: base})
	if err != nil || !strings.Contains(diff, "revision 7 → 6") || !strings.Contains(diff, "v2.3.0") {
		t.Fatalf("undo diff: %q %v", diff, err)
	}
	diff, err = p.DryRun(ctx, core.ActionSpec{Verb: kp.VerbScale, Params: with("replicas", "5")})
	if err != nil || !strings.Contains(diff, "3 → 5") {
		t.Fatalf("scale diff: %q %v", diff, err)
	}
	if _, err := p.DryRun(ctx, core.ActionSpec{Verb: kp.VerbRolloutRestart, Params: base}); err != nil {
		t.Fatal(err)
	}
	for _, a := range cs.Actions()[before:] {
		if v := a.GetVerb(); v != "get" && v != "list" {
			t.Fatalf("dry run issued a %s", v)
		}
	}
	if _, err := p.Execute(ctx, core.ActionSpec{Verb: kp.VerbRolloutRestart, Params: base}); !errors.Is(err, core.ErrExecutorDisabled) {
		t.Fatalf("execute must be disabled by default: %v", err)
	}
}

func TestExecuteWhenEnabled(t *testing.T) {
	p, cs := provider(t)
	p.ExecutorEnabled = func(context.Context) bool { return true }
	ctx := context.Background()
	params := map[string]string{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api"}
	if _, err := p.Execute(ctx, core.ActionSpec{Verb: kp.VerbRolloutRestart, Params: params}); err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("checkout").Get(ctx, "api", metav1.GetOptions{})
	if d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] == "" {
		t.Fatal("restart annotation not patched")
	}
	if _, err := p.Execute(ctx, core.ActionSpec{Verb: kp.VerbRolloutUndo, Params: params}); err != nil {
		t.Fatal(err)
	}
	d, _ = cs.AppsV1().Deployments("checkout").Get(ctx, "api", metav1.GetOptions{})
	if d.Spec.Template.Spec.Containers[0].Image != "registry/api:v2.3.0" {
		t.Fatalf("undo image: %s", d.Spec.Template.Spec.Containers[0].Image)
	}
	if _, ok := d.Spec.Template.Labels["pod-template-hash"]; ok {
		t.Fatal("pod-template-hash must be stripped on undo")
	}
	if _, err := p.Verify(ctx, core.ActionSpec{Verb: kp.VerbRolloutUndo, Params: params}); err == nil {
		t.Fatal("verify should report incomplete rollout (1/3 ready)")
	}
}
