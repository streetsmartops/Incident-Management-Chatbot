// Package kubernetes implements core.CloudProvider with client-go.
//
// v1 is read-only: the ServiceAccount has get/list/watch on pods, pods/log,
// events, deployments and replicasets only (deploy/k8s/rbac-readonly.yaml).
// DryRun computes diffs client-side from current state so it works with
// read-only RBAC. Execute is gated by an executor switch and returns
// core.ErrExecutorDisabled unless the remediation engine enabled it AND the
// write Role (deploy/k8s/rbac-remediation.yaml, not applied in v1) exists.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

const revisionAnnotation = "deployment.kubernetes.io/revision"

// Cluster is one configured cluster.
type Cluster struct {
	Name       string
	Client     k8s.Interface
	Namespaces []string // allow-list, "prefix-*" globs allowed
}

// Provider is the Kubernetes adapter.
type Provider struct {
	clusters map[string]Cluster
	// ExecutorEnabled is set by the remediation engine from the two-key kill switch.
	ExecutorEnabled func(ctx context.Context) bool
	Now             func() time.Time
}

// ClusterConfig describes how to connect.
type ClusterConfig struct {
	Name           string
	InCluster      bool
	KubeconfigPath string
	Context        string
	Namespaces     []string
}

// Connect builds clients for each configured cluster.
func Connect(cfgs []ClusterConfig) (*Provider, error) {
	var cls []Cluster
	for _, c := range cfgs {
		var rc *rest.Config
		var err error
		if c.InCluster {
			rc, err = rest.InClusterConfig()
		} else {
			rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				&clientcmd.ClientConfigLoadingRules{ExplicitPath: c.KubeconfigPath},
				&clientcmd.ConfigOverrides{CurrentContext: c.Context},
			).ClientConfig()
		}
		if err != nil {
			return nil, fmt.Errorf("kubernetes cluster %s: %w", c.Name, err)
		}
		rc.UserAgent = "incidentd"
		rc.Timeout = 15 * time.Second
		cs, err := k8s.NewForConfig(rc)
		if err != nil {
			return nil, fmt.Errorf("kubernetes cluster %s: %w", c.Name, err)
		}
		cls = append(cls, Cluster{Name: c.Name, Client: cs, Namespaces: c.Namespaces})
	}
	return New(cls), nil
}

// New builds a provider from ready clients (tests use the fake clientset).
func New(clusters []Cluster) *Provider {
	m := map[string]Cluster{}
	for _, c := range clusters {
		m[c.Name] = c
	}
	return &Provider{clusters: m, ExecutorEnabled: func(context.Context) bool { return false }, Now: time.Now}
}

func (p *Provider) Name() string { return "kubernetes" }

// Scopes returns configured cluster names.
func (p *Provider) Scopes() []string {
	out := make([]string, 0, len(p.clusters))
	for n := range p.clusters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ScopeError is returned for clusters/namespaces outside the allow-list.
type ScopeError struct{ Cluster, Namespace string }

func (e *ScopeError) Error() string {
	return fmt.Sprintf("cluster %q namespace %q is not in the configured allow-list", e.Cluster, e.Namespace)
}

func (p *Provider) client(cluster, ns string) (k8s.Interface, error) {
	c, ok := p.clusters[cluster]
	if !ok {
		return nil, &ScopeError{cluster, ns}
	}
	if !NamespaceAllowed(c.Namespaces, ns) {
		return nil, &ScopeError{cluster, ns}
	}
	return c.Client, nil
}

// NamespaceAllowed matches ns against an allow-list with "*" globs.
func NamespaceAllowed(allow []string, ns string) bool {
	if ns == "" {
		return false
	}
	for _, a := range allow {
		if ok, _ := path.Match(a, ns); ok {
			return true
		}
	}
	return false
}

func images(spec corev1.PodSpec) []string {
	var out []string
	for _, c := range spec.Containers {
		out = append(out, c.Name+"="+c.Image)
	}
	return out
}

func toWorkload(cluster string, d *appsv1.Deployment) core.Workload {
	w := core.Workload{
		Ref:      core.WorkloadRef{Cluster: cluster, Namespace: d.Namespace, Kind: "Deployment", Name: d.Name},
		Ready:    d.Status.ReadyReplicas,
		Updated:  d.Status.UpdatedReplicas,
		Images:   images(d.Spec.Template.Spec),
		Revision: d.Annotations[revisionAnnotation],
	}
	if d.Spec.Replicas != nil {
		w.Desired = *d.Spec.Replicas
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing || c.Type == appsv1.DeploymentAvailable {
			w.Condition += fmt.Sprintf("%s=%s(%s) ", c.Type, c.Status, c.Reason)
		}
	}
	w.Condition = strings.TrimSpace(w.Condition)
	return w
}

// ListWorkloads lists Deployments in a namespace.
func (p *Provider) ListWorkloads(ctx context.Context, cluster, ns string) ([]core.Workload, error) {
	cs, err := p.client(cluster, ns)
	if err != nil {
		return nil, err
	}
	list, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]core.Workload, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, toWorkload(cluster, &list.Items[i]))
	}
	return out, nil
}

// WorkloadEvents lists recent events for the deployment and its pods/replicasets.
func (p *Provider) WorkloadEvents(ctx context.Context, ref core.WorkloadRef, since time.Time) ([]core.ObsEvent, error) {
	cs, err := p.client(ref.Cluster, ref.Namespace)
	if err != nil {
		return nil, err
	}
	list, err := cs.CoreV1().Events(ref.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []core.ObsEvent
	for _, e := range list.Items {
		if !strings.HasPrefix(e.InvolvedObject.Name, ref.Name) {
			continue
		}
		at := e.LastTimestamp.Time
		if at.IsZero() {
			at = e.EventTime.Time
		}
		if at.IsZero() {
			at = e.CreationTimestamp.Time
		}
		if at.Before(since) {
			continue
		}
		out = append(out, core.ObsEvent{
			At:     at,
			Title:  fmt.Sprintf("%s %s/%s: %s", e.Type, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason),
			Text:   e.Message,
			Source: "kubernetes",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

func (p *Provider) replicaSets(ctx context.Context, cs k8s.Interface, d *appsv1.Deployment) ([]appsv1.ReplicaSet, error) {
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, err
	}
	list, err := cs.AppsV1().ReplicaSets(d.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	var out []appsv1.ReplicaSet
	for _, rs := range list.Items {
		for _, o := range rs.OwnerReferences {
			if o.UID == d.UID || (d.UID == "" && o.Name == d.Name) {
				out = append(out, rs)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return revOf(&out[i]) > revOf(&out[j]) })
	return out, nil
}

func revOf(rs *appsv1.ReplicaSet) int {
	n, _ := strconv.Atoi(rs.Annotations[revisionAnnotation])
	return n
}

// RolloutHistory returns revisions newest first.
func (p *Provider) RolloutHistory(ctx context.Context, ref core.WorkloadRef) ([]core.Revision, error) {
	cs, err := p.client(ref.Cluster, ref.Namespace)
	if err != nil {
		return nil, err
	}
	d, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	rss, err := p.replicaSets(ctx, cs, d)
	if err != nil {
		return nil, err
	}
	out := make([]core.Revision, 0, len(rss))
	for _, rs := range rss {
		out = append(out, core.Revision{Revision: rs.Annotations[revisionAnnotation], Images: images(rs.Spec.Template.Spec), Created: rs.CreationTimestamp.Time})
	}
	return out, nil
}

// PodLogs returns tail logs from up to 3 pods of the deployment.
func (p *Provider) PodLogs(ctx context.Context, ref core.WorkloadRef, since time.Time, tail int) ([]core.LogLine, error) {
	cs, err := p.client(ref.Cluster, ref.Namespace)
	if err != nil {
		return nil, err
	}
	d, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := cs.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	if tail <= 0 || tail > 500 {
		tail = 100
	}
	t := int64(tail)
	st := metav1.NewTime(since)
	var out []core.LogLine
	for i, pod := range pods.Items {
		if i >= 3 {
			break
		}
		stream, err := cs.CoreV1().Pods(ref.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &t, SinceTime: &st}).Stream(ctx)
		if err != nil {
			out = append(out, core.LogLine{At: p.Now(), Source: pod.Name, Status: "error", Message: "log fetch failed: " + err.Error()})
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(stream, 256<<10))
		stream.Close()
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if line != "" {
				out = append(out, core.LogLine{Source: pod.Name, Message: line})
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- Actions

// Supported catalog verbs.
const (
	VerbRolloutRestart = "rollout_restart"
	VerbScale          = "scale"
	VerbRolloutUndo    = "rollout_undo"
)

func (p *Provider) target(ctx context.Context, a core.ActionSpec) (k8s.Interface, *appsv1.Deployment, error) {
	cs, err := p.client(a.Params["cluster"], a.Params["namespace"])
	if err != nil {
		return nil, nil, err
	}
	d, err := cs.AppsV1().Deployments(a.Params["namespace"]).Get(ctx, a.Params["deployment"], metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	return cs, d, nil
}

// DryRun returns a human-readable diff without changing anything.
func (p *Provider) DryRun(ctx context.Context, a core.ActionSpec) (string, error) {
	cs, d, err := p.target(ctx, a)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s/%s/deployment/%s", a.Params["cluster"], d.Namespace, d.Name)
	switch a.Verb {
	case VerbRolloutRestart:
		return fmt.Sprintf("%s\n~ spec.template.metadata.annotations[kubectl.kubernetes.io/restartedAt]: %q → %q\n(pods are replaced per the rollout strategy; replicas %d)",
			id, d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"], p.Now().UTC().Format(time.RFC3339), deref(d.Spec.Replicas)), nil
	case VerbScale:
		n, err := strconv.Atoi(a.Params["replicas"])
		if err != nil {
			return "", fmt.Errorf("replicas must be an integer")
		}
		return fmt.Sprintf("%s\n~ spec.replicas: %d → %d", id, deref(d.Spec.Replicas), n), nil
	case VerbRolloutUndo:
		rss, err := p.replicaSets(ctx, cs, d)
		if err != nil {
			return "", err
		}
		prev, err := previous(rss, a.Params["toRevision"], d.Annotations[revisionAnnotation])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s\n~ revision %s → %s\n- images %v\n+ images %v", id, d.Annotations[revisionAnnotation], prev.Annotations[revisionAnnotation],
			images(d.Spec.Template.Spec), images(prev.Spec.Template.Spec)), nil
	}
	return "", fmt.Errorf("unsupported kubernetes verb %q", a.Verb)
}

func deref(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

func previous(rss []appsv1.ReplicaSet, toRevision, current string) (*appsv1.ReplicaSet, error) {
	for i := range rss {
		r := rss[i].Annotations[revisionAnnotation]
		if toRevision != "" {
			if r == toRevision {
				return &rss[i], nil
			}
			continue
		}
		if r != current {
			return &rss[i], nil
		}
	}
	return nil, fmt.Errorf("no previous revision found")
}

// Execute performs the action. Returns core.ErrExecutorDisabled unless enabled.
func (p *Provider) Execute(ctx context.Context, a core.ActionSpec) (string, error) {
	if p.ExecutorEnabled == nil || !p.ExecutorEnabled(ctx) {
		return "", core.ErrExecutorDisabled
	}
	cs, d, err := p.target(ctx, a)
	if err != nil {
		return "", err
	}
	deps := cs.AppsV1().Deployments(d.Namespace)
	switch a.Verb {
	case VerbRolloutRestart:
		patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, p.Now().UTC().Format(time.RFC3339))
		if _, err := deps.Patch(ctx, d.Name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: "incidentd"}); err != nil {
			return "", err
		}
		return "restart triggered", nil
	case VerbScale:
		n, err := strconv.Atoi(a.Params["replicas"])
		if err != nil {
			return "", fmt.Errorf("replicas must be an integer")
		}
		sc := &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace}, Spec: autoscalingv1.ScaleSpec{Replicas: int32(n)}}
		if _, err := deps.UpdateScale(ctx, d.Name, sc, metav1.UpdateOptions{FieldManager: "incidentd"}); err != nil {
			return "", err
		}
		return fmt.Sprintf("scaled to %d", n), nil
	case VerbRolloutUndo:
		rss, err := p.replicaSets(ctx, cs, d)
		if err != nil {
			return "", err
		}
		prev, err := previous(rss, a.Params["toRevision"], d.Annotations[revisionAnnotation])
		if err != nil {
			return "", err
		}
		tmpl := prev.Spec.Template.DeepCopy()
		delete(tmpl.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
		// Replace the whole template (as kubectl rollout undo does) so stale
		// labels/annotations are removed rather than merged.
		b, err := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec/template", "value": tmpl}})
		if err != nil {
			return "", err
		}
		if _, err := deps.Patch(ctx, d.Name, types.JSONPatchType, b, metav1.PatchOptions{FieldManager: "incidentd"}); err != nil {
			return "", err
		}
		return "rolled back to revision " + prev.Annotations[revisionAnnotation], nil
	}
	return "", fmt.Errorf("unsupported kubernetes verb %q", a.Verb)
}

// Verify checks rollout completion (updated == ready == desired).
func (p *Provider) Verify(ctx context.Context, a core.ActionSpec) (string, error) {
	_, d, err := p.target(ctx, a)
	if err != nil {
		return "", err
	}
	w := toWorkload(a.Params["cluster"], d)
	if w.Updated == w.Desired && w.Ready == w.Desired {
		return fmt.Sprintf("rollout complete: %d/%d ready", w.Ready, w.Desired), nil
	}
	return "", fmt.Errorf("rollout not complete: updated %d ready %d desired %d", w.Updated, w.Ready, w.Desired)
}

var _ core.CloudProvider = (*Provider)(nil)
