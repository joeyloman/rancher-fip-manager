package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMarkLeaderPodPublishesAndClearsLabel(t *testing.T) {
	const (
		podName      = "controller-0"
		podNamespace = "rancher-fip-manager"
	)

	clientset := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: podNamespace},
	})

	uiReady := make(chan struct{})
	close(uiReady)

	markLeaderPod(context.Background(), clientset, podName, podNamespace, uiReady)

	pod, err := clientset.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pod.Labels[leaderLabelKey]; got != "true" {
		t.Fatalf("leader label = %q, want %q", got, "true")
	}

	removeLeaderLabel(context.Background(), clientset, podName, podNamespace)

	pod, err = clientset.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pod.Labels[leaderLabelKey]; ok {
		t.Fatalf("leader label was not removed: %v", pod.Labels)
	}
}
