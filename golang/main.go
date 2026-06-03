package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// DeploymentHealth represents the health status of a single Deployment.
type DeploymentHealth struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	DesiredReplicas int32  `json:"desired_replicas"`
	ReadyReplicas   int32  `json:"ready_replicas"`
	Healthy         bool   `json:"healthy"`
}

// DeploymentHealthResponse is the envelope returned by GET /deployments/health.
type DeploymentHealthResponse struct {
	Deployments []DeploymentHealth `json:"deployments"`
	AllHealthy  bool               `json:"all_healthy"`
}

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig, leave empty for in-cluster")
	listenAddr := flag.String("address", ":8080", "HTTP server listen address")
	flag.Parse()

	kConfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to build kubeconfig: %v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(kConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to create kubernetes client: %v\n", err)
		os.Exit(1)
	}

	version, err := getKubernetesVersion(clientset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to connect to Kubernetes API: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Connected to Kubernetes %s\n", version)

	if err := startServer(*listenAddr, clientset); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: server exited: %v\n", err)
		os.Exit(1)
	}
}

// getKubernetesVersion returns a string GitVersion of the Kubernetes server defined by the clientset.
//
// If it can't connect an error will be returned, which makes it useful to check connectivity.
func getKubernetesVersion(clientset kubernetes.Interface) (string, error) {
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return version.String(), nil
}

// startServer launches an HTTP server with defined handlers and blocks until it's terminated or fails with an error.
// It handles SIGTERM/SIGINT gracefully, allowing in-flight requests up to 10 seconds to complete.
func startServer(listenAddr string, clientset kubernetes.Interface) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler(clientset))
	mux.HandleFunc("/deployments/health", deploymentHealthHandler(clientset))

	srv := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	serverErr := make(chan error, 1)
	go func() {
		fmt.Printf("Server listening on %s\n", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return err
	case sig := <-quit:
		fmt.Printf("Received signal %s, shutting down gracefully...\n", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	fmt.Println("Server stopped cleanly")
	return nil
}

// healthHandler responds with the health status of the application.
//
// Task 2: The handler actively probes the Kubernetes API server on every request.
// Returns 200 OK if reachable, 503 Service Unavailable if not.
func healthHandler(clientset kubernetes.Interface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := getKubernetesVersion(clientset)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(fmt.Sprintf("unhealthy: k8s api unreachable: %v", err)))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}

// deploymentHealthHandler returns an HTTP handler that checks all Deployments
// across all namespaces and reports whether each has the expected number of ready pods.
//
// Task 1: A Deployment is considered healthy when readyReplicas == spec.replicas.
// Supports ?format=table for terminal output, ?format=html for browser grid, defaults to JSON.
// Returns 200 if all healthy, 503 if any deployment is degraded.
func deploymentHealthHandler(clientset kubernetes.Interface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		deploymentList, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to list deployments: %v", err), http.StatusInternalServerError)
			return
		}

		response := DeploymentHealthResponse{
			Deployments: make([]DeploymentHealth, 0, len(deploymentList.Items)),
			AllHealthy:  true,
		}

		for _, d := range deploymentList.Items {
			// spec.replicas defaults to 1 when not explicitly set.
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}

			ready := d.Status.ReadyReplicas
			healthy := ready == desired

			if !healthy {
				response.AllHealthy = false
			}

			response.Deployments = append(response.Deployments, DeploymentHealth{
				Namespace:       d.Namespace,
				Name:            d.Name,
				DesiredReplicas: desired,
				ReadyReplicas:   ready,
				Healthy:         healthy,
			})
		}

		// Return 503 if any deployment is degraded — lets load balancers and
		// monitoring tools detect cluster health by status code alone.
		if !response.AllHealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		format := r.URL.Query().Get("format")
		switch format {

		case "table":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "%-20s %-25s %-10s %-10s %s\n", "NAMESPACE", "NAME", "DESIRED", "READY", "HEALTHY")
			fmt.Fprintf(w, "%-20s %-25s %-10s %-10s %s\n", "---", "---", "---", "---", "---")
			for _, d := range response.Deployments {
				status := "✓"
				if !d.Healthy {
					status = "✗"
				}
				fmt.Fprintf(w, "%-20s %-25s %-10d %-10d %s\n",
					d.Namespace, d.Name, d.DesiredReplicas, d.ReadyReplicas, status)
			}

		case "html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")

			total := len(response.Deployments)
			degraded := 0
			for _, d := range response.Deployments {
				if !d.Healthy {
					degraded++
				}
			}
			healthy := total - degraded

			fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Deployment Health</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; font-size: 18px; background: #f5f5f5; color: #1a1a1a; padding: 2.5rem; }
  h1 { font-size: 1.6rem; font-weight: 600; margin-bottom: 1.75rem; color: #1a1a1a; }
  .summary { display: flex; gap: 1.25rem; margin-bottom: 1.75rem; }
  .card { background: #fff; border: 1px solid #e5e5e5; border-radius: 8px; padding: 1.25rem 2rem; min-width: 150px; }
  .card-label { font-size: 0.85rem; color: #1a1a1a; text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 0.4rem; }
  .card-value { font-size: 2rem; font-weight: 600; }
  .card-value.ok  { color: #16a34a; }
  .card-value.bad { color: #d97706; }
  .card-value.neutral { color: #1a1a1a; }
  .table-wrap { background: #fff; border: 1px solid #e5e5e5; border-radius: 8px; overflow: hidden; }
  table { width: 100%%; border-collapse: collapse; font-size: 1rem; }
  th { background: #f9f9f9; color: #1a1a1a; font-weight: 500; font-size: 0.85rem; text-transform: uppercase; letter-spacing: 0.05em; padding: 1rem 1.25rem; text-align: left; border-bottom: 1px solid #e5e5e5; }
  td { padding: 1rem 1.25rem; border-bottom: 1px solid #f0f0f0; }
  tr:last-child td { border-bottom: none; }
  tr:hover td { background: #fafafa; }
  .ns { color: #1a1a1a; font-size: 0.95rem; }
  .num { text-align: right; font-variant-numeric: tabular-nums; color: #1a1a1a; }
  .num.bad { color: #d97706; font-weight: 600; }
  .badge { display: inline-flex; align-items: center; gap: 6px; font-size: 0.9rem; font-weight: 500; padding: 4px 14px; border-radius: 9999px; }
  .badge.ok  { background: #dcfce7; color: #15803d; }
  .badge.bad { background: #fef3c7; color: #92400e; }
  .dot { width: 8px; height: 8px; border-radius: 50%%; }
  .dot.ok  { background: #16a34a; }
  .dot.bad { background: #d97706; }
  .ts { font-size: 0.9rem; color: #1a1a1a; margin-top: 1.75rem; }
</style>
</head>
<body>
<h1>Deployment Health</h1>
<div class="summary">
  <div class="card"><div class="card-label">Total</div><div class="card-value neutral">%d</div></div>
  <div class="card"><div class="card-label">Healthy</div><div class="card-value ok">%d</div></div>
  <div class="card"><div class="card-label">Degraded</div><div class="card-value bad">%d</div></div>
</div>
<div class="table-wrap">
<table>
  <thead>
    <tr>
      <th>Namespace</th>
      <th>Name</th>
      <th style="text-align:right">Desired</th>
      <th style="text-align:right">Ready</th>
      <th>Status</th>
    </tr>
  </thead>
  <tbody>
`, total, healthy, degraded)

			for _, d := range response.Deployments {
				badgeClass := "ok"
				badgeText := "Healthy"
				readyClass := "num"
				if !d.Healthy {
					badgeClass = "bad"
					badgeText = "Degraded"
					readyClass = "num bad"
				}
				fmt.Fprintf(w,
					`    <tr>
      <td class="ns">%s</td>
      <td>%s</td>
      <td class="num">%d</td>
      <td class="%s">%d</td>
      <td><span class="badge %s"><span class="dot %s"></span>%s</span></td>
    </tr>
`,
					d.Namespace, d.Name, d.DesiredReplicas, readyClass, d.ReadyReplicas,
					badgeClass, badgeClass, badgeText)
			}

			fmt.Fprintf(w, `  </tbody>
</table>
</div>
<p class="ts">Auto-refreshes every 10s &mdash; <a href="/deployments/health">JSON</a> &middot; <a href="/deployments/health?format=table">Table</a></p>
<script>setTimeout(() => location.reload(), 10000);</script>
</body>
</html>`)

		default:
			pretty, err := json.MarshalIndent(response, "", "  ")
			if err != nil {
				http.Error(w, "failed to encode response", http.StatusInternalServerError)
				return
			}
			// If the request comes from a browser it sends Accept: text/html — serve a styled page.
			// curl and API clients omit that header and get compact JSON.
			accept := r.Header.Get("Accept")
			if len(accept) >= 9 && accept[:9] == "text/html" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Deployment Health — JSON</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #f5f5f5; padding: 2rem; }
  h1 { font-size: 1.25rem; font-weight: 600; margin-bottom: 1rem; color: #1a1a1a; }
  .links { font-size: 0.9rem; margin-bottom: 1.5rem; }
  .links a { color: #2563eb; text-decoration: none; margin-right: 1rem; }
  pre { background: #1e1e1e; color: #d4d4d4; padding: 1.5rem; border-radius: 8px; font-size: 0.95rem; line-height: 1.6; overflow-x: auto; white-space: pre-wrap; }
  .key    { color: #9cdcfe; }
  .str    { color: #ce9178; }
  .num    { color: #b5cea8; }
  .bool-t { color: #4ec9b0; }
  .bool-f { color: #f97583; }
</style>
</head>
<body>
<h1>Deployment Health — JSON</h1>
<div class="links">
  <a href="/deployments/health?format=html">&#8592; Grid view</a>
  <a href="/deployments/health?format=table">Table view</a>
</div>
<pre id="out"></pre>
<script>
const data = %s;
const json = JSON.stringify(data, null, 2);
document.getElementById("out").innerHTML = json.replace(
  /("(\\u[a-zA-Z0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+\-]?\d+)?)/g,
  m => {
    if (/^"/.test(m)) return /:$/.test(m) ? '<span class="key">'+m+'</span>' : '<span class="str">'+m+'</span>';
    if (/true/.test(m))  return '<span class="bool-t">'+m+'</span>';
    if (/false/.test(m)) return '<span class="bool-f">'+m+'</span>';
    return '<span class="num">'+m+'</span>';
  }
);
</script>
</body>
</html>`, string(pretty))
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.Write(pretty)
			}
		}
	}
}
