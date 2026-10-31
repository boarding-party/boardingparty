package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

func podReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// forward port-forwards to a Ready pod matching podSelector in ns and returns
// the local base URL. The admin API is loopback-only inside the pod, so we go
// to the pod, not a Service.
func forward(ctx context.Context, cfg *rest.Config, ns, podSelector string, remotePort int) (string, func(), error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", nil, err
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: podSelector})
	if err != nil {
		return "", nil, fmt.Errorf("list director pods in %s: %w", ns, err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if podReady(&pods.Items[i]) {
			pod = &pods.Items[i]
			break
		}
	}
	if pod == nil {
		return "", nil, fmt.Errorf("no ready director pod in namespace %s — is boardingparty installed?", ns)
	}
	rt, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return "", nil, err
	}
	u := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod.Name).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)
	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:" + strconv.Itoa(remotePort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return "", nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- pf.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-errCh:
		return "", nil, fmt.Errorf("port-forward: %w", err)
	case <-ctx.Done():
		close(stopCh)
		return "", nil, ctx.Err()
	}
	ports, err := pf.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopCh)
		return "", nil, fmt.Errorf("port-forward: no local port: %v", err)
	}
	stop := func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	}
	return "http://127.0.0.1:" + strconv.Itoa(int(ports[0].Local)), stop, nil
}
