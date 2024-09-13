package controllers

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	networkingv1alpha3 "istio.io/client-go/pkg/apis/networking/v1alpha3"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
    client.Client
    Scheme *runtime.Scheme
    KubeClient  kubernetes.Interface
}

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices,verbs=get;list;watch;update;patch

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    // Only handle Services in istio-system and those that are acme-http-solvers
    if req.Namespace != "istio-system" || !strings.HasPrefix(req.Name, "cm-acme-http-solver-") {
        return ctrl.Result{}, nil
    }

    // Fetch the Service
    var service corev1.Service
    if err := r.Get(ctx, req.NamespacedName, &service); err != nil {
        log.Error(err, "unable to fetch Service", "service", req.NamespacedName)
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Get Pods for the Service using its selector
    podList := &corev1.PodList{}
    selector := client.MatchingLabels(service.Spec.Selector)
    if err := r.List(ctx, podList, client.InNamespace(req.Namespace), selector); err != nil {
        log.Error(err, "failed to list pods for Service", "Service", service.Name)
        return ctrl.Result{}, err
    }

    // Assume the first Pod's logs contain the domain; real-world use may need more robust handling
    if len(podList.Items) == 0 {
        log.Info("No pods found for Service", "Service", service.Name)
        return ctrl.Result{}, nil
    }
    pod := podList.Items[0]

    // Extract domain from pod logs
    domain, err := r.extractDomainFromLogs(&pod)
    if err != nil {
        log.Error(err, "failed to extract domain from pod logs", "Pod", pod.Name)
        return ctrl.Result{}, err
    }

    // Update VirtualServices with the extracted domain
    return r.updateVirtualServices(ctx, domain, service.Name)
}

// updateVirtualServices finds and updates the VirtualService for the given domain
func (r *ServiceReconciler) updateVirtualServices(ctx context.Context, domain, serviceName string) (ctrl.Result, error) {
    log := log.FromContext(ctx) // Create the logger from the context

    var virtualServices networkingv1alpha3.VirtualServiceList
    if err := r.List(ctx, &virtualServices); err != nil {
        return ctrl.Result{}, err
    }

    for _, vs := range virtualServices.Items {
        for _, host := range vs.Spec.Hosts {
            if host == domain {
                updated := vs.DeepCopy()
                for i, http := range updated.Spec.Http {
                    if http.Match != nil && http.Match[0].Uri.GetPrefix() == "/.well-known/acme-challenge/" {
                        updated.Spec.Http[i].Route[0].Destination.Host = serviceName + ".istio-system.svc.cluster.local"
                        if err := r.Update(ctx, updated); err != nil {
                            return ctrl.Result{}, err
                        }
                        log.Info("Updated VirtualService with new service", "VirtualService", updated.Name, "Service", serviceName)
                        break
                    }
                }
                break // Break after updating the matching VirtualService
            }
        }
    }

    return ctrl.Result{}, nil
}

// extractDomainFromLogs fetches and parses the expected_domain from pod logs
func (r *ServiceReconciler) extractDomainFromLogs(pod *corev1.Pod) (string, error) {
    logOpts := &corev1.PodLogOptions{}
    req := r.KubeClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, logOpts)
    logStream, err := req.Stream(context.TODO())
    if err != nil {
        return "", err
    }
    defer logStream.Close()

    scanner := bufio.NewScanner(logStream)
    for scanner.Scan() {
        line := scanner.Text()
        if strings.Contains(line, "expected_domain") {
            // Simple parsing based on log structure
            parts := strings.Split(line, " ")
            for _, part := range parts {
                if strings.HasPrefix(part, "expected_domain=") {
                    domain := strings.TrimPrefix(part, "expected_domain=")
                    domain = strings.Trim(domain, "\"")
                    return domain, nil
                }
            }
        }
    }
    if err := scanner.Err(); err != nil {
        return "", err
    }
    return "", fmt.Errorf("domain not found in pod logs")
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&corev1.Service{}).
        Complete(r)
}
