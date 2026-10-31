package controller

// Agent and UI pods are no longer deleted when they are stuck. They are
// diagnosed, and the diagnosis is reported where an operator looks: the
// AgentReady/UIReady condition's reason and message, and a Warning event.
//
// The deletion this replaces (deleteStalePodsForDeployment) removed any pod
// that was Pending for 2 minutes or in ImagePullBackOff/CrashLoopBackOff, on
// every reconcile. None of those cases is fixed by recreating the pod:
//
//   - A pod still pulling a large image is Pending (ContainerCreating). It
//     was deleted mid-pull every 2 minutes, so on a slow registry or a cold
//     node it could never start.
//   - A crashlooping pod restarts the same image with the same config. The
//     deletion threw away `oc logs --previous`, reset the kubelet's restart
//     backoff (so it restarted faster, not slower), and recorded a
//     "SelfHealed" event every 30 seconds for something that healed nothing.
//   - ImagePullBackOff from a bad tag or a missing pull secret re-pulled
//     every 30 seconds instead of backing off, which hits registry rate
//     limits, and never succeeded.
//   - An unschedulable pod with the current spec is just as unschedulable
//     when recreated.
//
// The case the deletion was written for, the agent's Recreate rollout
// stuck on a bad new image with no old pod to fall back to, is handled by
// automatic rollback (status.go's reconcileAutoRollback), which restores
// the last healthy image. Deleting the new pod never brought the old one
// back.

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// podProblem is why a pod is not becoming ready, in the kubelet's words.
type podProblem struct {
	Pod     string
	Reason  string
	Message string
}

func (p podProblem) String() string {
	if p.Message == "" {
		return fmt.Sprintf("pod %s: %s", p.Pod, p.Reason)
	}
	return fmt.Sprintf("pod %s: %s: %s", p.Pod, p.Reason, p.Message)
}

// problemWaitingReasons are container Waiting reasons that mean the pod will
// not become ready without a change to the image, its config or the
// cluster. Ordinary startup reasons (ContainerCreating, PodInitializing) are
// deliberately absent: they are progress, not problems.
var problemWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CrashLoopBackOff":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

// diagnosePod reports why pod is not becoming ready, or nil when nothing is
// wrong beyond normal startup.
func diagnosePod(pod *corev1.Pod) *podProblem {
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		w := cs.State.Waiting
		if w == nil || !problemWaitingReasons[w.Reason] {
			continue
		}
		msg := strings.TrimSpace(w.Message)
		if w.Reason == "CrashLoopBackOff" {
			// The crash itself is in the last terminated state, not in the
			// back-off message.
			if t := cs.LastTerminationState.Terminated; t != nil {
				detail := fmt.Sprintf("container %s exited with code %d", cs.Name, t.ExitCode)
				if t.Reason != "" {
					detail += " (" + t.Reason + ")"
				}
				if m := strings.TrimSpace(t.Message); m != "" {
					detail += ": " + m
				}
				msg = detail
			}
		}
		return &podProblem{Pod: pod.Name, Reason: w.Reason, Message: truncate(msg, 300)}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return &podProblem{Pod: pod.Name, Reason: "Unschedulable", Message: truncate(strings.TrimSpace(c.Message), 300)}
		}
	}
	return nil
}

// diagnoseDeploymentPods returns the first problem among the pods of the
// Deployment whose pod template carries {"app": deploymentName}, or nil.
func diagnoseDeploymentPods(ctx context.Context, c client.Client, namespace, deploymentName string) (*podProblem, error) {
	podList := &corev1.PodList{}
	if err := c.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{"app": deploymentName}); err != nil {
		return nil, err
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if p := diagnosePod(pod); p != nil {
			return p, nil
		}
	}
	return nil, nil
}

// restartedAtAnnotation is what `oc rollout restart` sets on a pod template.
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// keepRestartedAt carries `oc rollout restart`'s annotation from the
// Deployment's current pod template into the one the operator writes. The
// agent and UI reconcilers rebuild the template from scratch, which dropped
// it on the next reconcile: a second rollout for the agent (Recreate, so a
// second outage) and a restart undone part way through for the UI.
func keepRestartedAt(previous, desired *corev1.PodTemplateSpec) {
	v, ok := previous.Annotations[restartedAtAnnotation]
	if !ok {
		return
	}
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	desired.Annotations[restartedAtAnnotation] = v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
