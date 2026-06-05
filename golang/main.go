// Package main is the entry point for the SRE tool.
// This tool connects to a Kubernetes cluster and exposes an HTTP server
// with endpoints that help the SRE team monitor cluster health.
package main

import (
	// "context" lets us pass cancellation signals and deadlines to API calls.
	// Think of it as a way to say "if this takes too long, give up".
	"context"

	// "encoding/json" converts Go structs into JSON text (and back).
	// We use it to write JSON responses to the HTTP client.
	"encoding/json"

	// "flag" parses command-line arguments like --kubeconfig or --address.
	"flag"

	// "fmt" is the standard formatting package — used for Printf, Sprintf, Errorf.
	"fmt"

	// "html/template" renders HTML pages safely from Go structs.
	// It automatically escapes values inserted into the HTML to prevent
	// cross-site scripting (XSS) attacks.
	"html/template"

	// "net/http" is Go's built-in HTTP server library.
	"net/http"

	// "time" provides time measurement — used to calculate API probe latency.
	"time"

	// metav1 contains common Kubernetes API types, like ListOptions.
	// The alias "metav1" is conventional shorthand for this package.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// "kubernetes" is the client library for talking to the Kubernetes API.
	"k8s.io/client-go/kubernetes"

	// "clientcmd" reads kubeconfig files and builds a connection config from them.
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	// Define a command-line flag called --kubeconfig.
	// When left empty, the tool assumes it's running inside the cluster
	// and uses the in-cluster service-account credentials automatically.
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig, leave empty for in-cluster")

	// Define a command-line flag called --address.
	// This is the host:port the HTTP server will listen on.
	listenAddr := flag.String("address", ":8080", "HTTP server listen address")

	// Actually parse the command-line flags provided by the user.
	flag.Parse()

	// Build a Kubernetes REST config from the kubeconfig file path.
	// This config contains the cluster URL, credentials, and TLS settings.
	kConfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		// panic stops the program immediately and prints the error.
		// For startup errors, this is acceptable — there's nothing to clean up.
		panic(err)
	}

	// Create a clientset using the config above.
	// A clientset is the main object we use to talk to Kubernetes.
	// It has methods for every Kubernetes resource type (Deployments, Pods, etc.).
	clientset, err := kubernetes.NewForConfig(kConfig)
	if err != nil {
		panic(err)
	}

	// Try fetching the Kubernetes server version.
	// This also acts as a connectivity check — if the cluster is unreachable,
	// we fail fast here rather than silently starting a broken server.
	version, err := getKubernetesVersion(clientset)
	if err != nil {
		panic(err)
	}

	// Print a confirmation message so the operator knows which cluster we connected to.
	fmt.Printf("Connected to Kubernetes %s\n", version)

	// Start the HTTP server. This call blocks until the server stops or crashes.
	// We pass the clientset so the HTTP handlers can use it to call the Kubernetes API.
	if err := startServer(*listenAddr, clientset); err != nil {
		panic(err)
	}
}

// getKubernetesVersion returns a string GitVersion of the Kubernetes server defined by the clientset.
//
// It calls the Kubernetes Discovery API, which returns metadata about the cluster
// including its version. If the cluster is unreachable, this returns an error,
// which makes it useful to check connectivity on startup.
func getKubernetesVersion(clientset kubernetes.Interface) (string, error) {
	// ServerVersion() calls the /version endpoint on the Kubernetes API server.
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		// Return an empty string and the error so the caller can decide what to do.
		return "", err
	}
	// version.String() returns something like "v1.26.3".
	return version.String(), nil
}

// startServer registers all HTTP route handlers and starts listening for requests.
// It blocks until the server shuts down or encounters a fatal error.
//
// listenAddr is a host:port string, e.g. ":8080" means "listen on all interfaces, port 8080".
// clientset is passed in so HTTP handlers can call the Kubernetes API.
func startServer(listenAddr string, clientset kubernetes.Interface) error {
	// Register the /healthz route.
	// healthHandler is a factory — we pass the clientset so it can probe the
	// Kubernetes API server on every request and report real connectivity status.
	http.HandleFunc("/healthz", healthHandler(clientset))

	// Register the /deployments/health route.
	// deploymentsHealthHandler is a function that *returns* a handler function,
	// so we call it here (with the clientset) to get the actual handler.
	// This single handler serves both JSON and HTML depending on ?format=html.
	http.HandleFunc("/deployments/health", deploymentsHealthHandler(clientset))

	fmt.Printf("Server listening on %s\n", listenAddr)

	// ListenAndServe starts the HTTP server. It only returns if something goes wrong.
	return http.ListenAndServe(listenAddr, nil)
}

// HealthzStatus is the JSON response body returned by GET /healthz.
// It reports whether the tool can successfully reach the Kubernetes API server.
type HealthzStatus struct {
	// Status is either "ok" (API reachable) or "degraded" (API unreachable).
	Status string `json:"status"`

	// APIServer is either "reachable" or "unreachable".
	APIServer string `json:"apiServer"`

	// LatencyMs is the round-trip time of the API probe in milliseconds.
	// It is 0 when the probe fails before a response is received.
	LatencyMs int64 `json:"latencyMs"`

	// Error contains the error message when the API is unreachable.
	// It is omitted from the JSON output when empty.
	Error string `json:"error,omitempty"`

	// CheckedAt is the UTC timestamp when the probe was performed.
	CheckedAt string `json:"checkedAt"`
}

// healthzHTMLTemplate is the HTML dashboard for GET /healthz?format=html.
// It shows a large coloured status indicator, the probe latency, and the
// timestamp of the last check. The page auto-refreshes every 10 seconds.
var healthzHTMLTemplate = template.Must(template.New("healthz").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>API Server Health</title>
	<meta http-equiv="refresh" content="10">
	<style>
		*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

		body {
			font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
			font-size: 16px;
			color: #000;
			background: #f5f5f5;
			padding: 32px;
		}

		h1 {
			font-size: 26px;
			font-weight: 600;
			margin-bottom: 24px;
			color: #000;
		}

		/* ── Status card ────────────────────────────────────────────────────── */
		.status-card {
			background: #fff;
			border: 1px solid #e0e0e0;
			border-radius: 6px;
			padding: 32px 40px;
			display: inline-flex;
			flex-direction: column;
			gap: 20px;
			margin-bottom: 28px;
			min-width: 380px;
		}

		/* The large status indicator line */
		.status-indicator {
			display: flex;
			align-items: center;
			gap: 14px;
			font-size: 24px;
			font-weight: 700;
		}

		/* Coloured dot */
		.dot {
			width: 20px;
			height: 20px;
			border-radius: 50%;
			flex-shrink: 0;
		}
		.dot.ok       { background: #16a34a; }
		.dot.degraded { background: #dc2626; }

		.status-text.ok       { color: #16a34a; }
		.status-text.degraded { color: #dc2626; }

		/* Detail rows below the indicator */
		.detail-row {
			display: flex;
			gap: 12px;
			font-size: 16px;
			color: #000;
		}

		.detail-label {
			font-weight: 600;
			min-width: 100px;
			color: #000;
		}

		.detail-value {
			color: #000;
		}

		/* Error box — only shown when the API is unreachable */
		.error-box {
			background: #fef2f2;
			border: 1px solid #fecaca;
			border-radius: 4px;
			padding: 12px 16px;
			font-size: 15px;
			color: #dc2626;
			font-family: "Courier New", Consolas, monospace;
			word-break: break-all;
		}

		/* ── Footer ─────────────────────────────────────────────────────────── */
		.footer {
			margin-top: 16px;
			font-size: 16px;
			font-weight: 500;
			color: #000;
		}

		.footer a       { color: #000; font-weight: 600; text-decoration: none; }
		.footer a:hover { text-decoration: underline; }
	</style>
</head>
<body>

<h1>API Server Health</h1>

<div class="status-card">

	{{ if eq .Status "ok" }}
	<div class="status-indicator">
		<div class="dot ok"></div>
		<span class="status-text ok">API Server Reachable</span>
	</div>
	{{ else }}
	<div class="status-indicator">
		<div class="dot degraded"></div>
		<span class="status-text degraded">API Server Unreachable</span>
	</div>
	{{ end }}

	<div class="detail-row">
		<span class="detail-label">Latency</span>
		<span class="detail-value">{{ .LatencyMs }} ms</span>
	</div>

	<div class="detail-row">
		<span class="detail-label">Checked at</span>
		<span class="detail-value">{{ .CheckedAt }}</span>
	</div>

	{{ if .Error }}
	<div class="error-box">{{ .Error }}</div>
	{{ end }}

</div>

<p class="footer">
	Auto-refreshes every 10s &mdash;
	<a href="/healthz">JSON</a> &middot;
	HTML
</p>

</body>
</html>
`))

// healthHandler is a handler factory that returns an http.HandlerFunc.
//
// On every request it probes the Kubernetes API server by calling
// clientset.Discovery().ServerVersion() — the same lightweight /version
// endpoint used at startup. It measures the round-trip latency and returns:
//
//   - 200 OK + JSON/HTML  when the API server responds successfully
//   - 503 Service Unavailable + JSON/HTML  when the API is unreachable
//
// The response format is selected via the ?format= query parameter:
//   - ?format=html  → self-refreshing HTML dashboard
//   - (default)     → machine-readable JSON
func healthHandler(clientset kubernetes.Interface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Record the time before the probe so we can calculate latency.
		start := time.Now()

		// Probe the Kubernetes API server.
		// Discovery().ServerVersion() calls GET /version on the API server.
		// This is the lightest authenticated call available — it returns a small
		// JSON object with the cluster version and requires no list permissions.
		_, err := clientset.Discovery().ServerVersion()

		// Calculate how long the probe took in milliseconds.
		latencyMs := time.Since(start).Milliseconds()

		// Build the response based on whether the probe succeeded or failed.
		var status HealthzStatus
		var statusCode int

		if err == nil {
			// The API server responded — the tool is fully operational.
			status = HealthzStatus{
				Status:    "ok",
				APIServer: "reachable",
				LatencyMs: latencyMs,
				CheckedAt: time.Now().UTC().Format(time.RFC3339),
			}
			statusCode = http.StatusOK // 200
		} else {
			// The API server did not respond — the tool cannot talk to Kubernetes.
			status = HealthzStatus{
				Status:    "degraded",
				APIServer: "unreachable",
				LatencyMs: 0,
				Error:     err.Error(),
				CheckedAt: time.Now().UTC().Format(time.RFC3339),
			}
			statusCode = http.StatusServiceUnavailable // 503
		}

		// Serve the response in the requested format.
		if r.URL.Query().Get("format") == "html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(statusCode)
			if tmplErr := healthzHTMLTemplate.Execute(w, status); tmplErr != nil {
				fmt.Printf("failed rendering healthz template: %v\n", tmplErr)
			}
			return
		}

		// Default: JSON response.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if jsonErr := json.NewEncoder(w).Encode(status); jsonErr != nil {
			fmt.Printf("failed encoding healthz JSON: %v\n", jsonErr)
		}
	}
}

// DeploymentStatus holds the health information for a single Kubernetes Deployment.
//
// The `json:"..."` tags tell the encoding/json package what key names to use
// when serialising this struct to JSON. Without them, Go would use the field
// names directly (e.g. "DesiredReplicas" instead of "desiredReplicas").
type DeploymentStatus struct {
	// Name is the name of the Deployment as it appears in Kubernetes.
	Name string `json:"name"`

	// Namespace is the Kubernetes namespace the Deployment lives in.
	// Namespaces are used to group and isolate resources within a cluster.
	Namespace string `json:"namespace"`

	// DesiredReplicas is the number of pod replicas requested in the Deployment spec.
	// This is what the operator *asked for*.
	DesiredReplicas int32 `json:"desiredReplicas"`

	// ReadyReplicas is the number of pods that are currently passing their readiness checks.
	// This is what is *actually running and healthy*.
	ReadyReplicas int32 `json:"readyReplicas"`

	// Healthy is true only when ReadyReplicas equals DesiredReplicas AND DesiredReplicas > 0.
	// A deployment scaled to zero is treated as unhealthy because no pods are serving traffic.
	Healthy bool `json:"healthy"`
}

// DeploymentsHealthReport is the top-level JSON object returned by GET /deployments/health.
// It contains a summary flag and the per-deployment breakdown.
type DeploymentsHealthReport struct {
	// Deployments is a list of health details for every Deployment found in the cluster,
	// across all namespaces.
	Deployments []DeploymentStatus `json:"deployments"`

	// AllHealthy is a convenience flag: true only when *every* deployment is healthy.
	// Callers can check this single field instead of looping through Deployments.
	AllHealthy bool `json:"allHealthy"`
}

// getDeploymentsHealth queries the Kubernetes API for all Deployments across every namespace
// and checks whether each one has the expected number of ready pods.
//
// ctx (context) carries a deadline and cancellation signal. Passing r.Context() from an
// HTTP request means that if the HTTP client disconnects, the Kubernetes API call is also
// cancelled automatically, freeing up resources.
//
// clientset is the Kubernetes API client used to list Deployments.
func getDeploymentsHealth(ctx context.Context, clientset kubernetes.Interface) (*DeploymentsHealthReport, error) {
	// List all Deployments in all namespaces.
	// metav1.NamespaceAll is an empty string "" which Kubernetes interprets as "all namespaces".
	// metav1.ListOptions{} means "no filters — give me everything".
	deploymentList, err := clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		// Wrap the error with extra context so callers know where it came from.
		// The %w verb lets the error be unwrapped later with errors.Is / errors.As.
		return nil, fmt.Errorf("listing deployments: %w", err)
	}

	// Build the report struct.
	// We pre-allocate the Deployments slice with the right capacity to avoid
	// unnecessary memory re-allocations as we append to it.
	// AllHealthy starts as true and is flipped to false the moment we find a problem.
	report := &DeploymentsHealthReport{
		Deployments: make([]DeploymentStatus, 0, len(deploymentList.Items)),
		AllHealthy:  true,
	}

	// Loop over every Deployment returned by the API.
	for _, d := range deploymentList.Items {
		// d.Spec.Replicas is a pointer (*int32), not a plain int32.
		// Kubernetes uses pointers so that "0 replicas" and "not set" can be distinguished.
		// When it is nil (not set), the Kubernetes default is 1 replica.
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas // dereference the pointer to get the actual value
		}

		// d.Status.ReadyReplicas is the live count reported by Kubernetes.
		ready := d.Status.ReadyReplicas

		// A deployment is healthy only if it has at least one desired replica
		// AND all desired replicas are ready.
		healthy := desired > 0 && ready == desired

		// If even one deployment is unhealthy, the whole report is unhealthy.
		if !healthy {
			report.AllHealthy = false
		}

		// Append this deployment's status to the report.
		report.Deployments = append(report.Deployments, DeploymentStatus{
			Name:            d.Name,
			Namespace:       d.Namespace,
			DesiredReplicas: desired,
			ReadyReplicas:   ready,
			Healthy:         healthy,
		})
	}

	// Return a pointer to the report (and no error).
	return report, nil
}

// deploymentsHealthHandler is a *handler factory*: it accepts the clientset and
// returns a function that matches the http.HandlerFunc signature (w, r).
// This pattern is used when a handler needs access to something that is created
// once at startup (the clientset) but used on every request.
//
// The handler supports three response formats selected via the ?format= query parameter:
//   - ?format=html   ->  card + badge dashboard, auto-refreshes every 10s
//   - ?format=table  ->  Excel-style spreadsheet grid, auto-refreshes every 10s
//   - (default)      ->  machine-readable JSON (used by scripts, monitoring tools, etc.)
//
// HTTP status codes:
//   - 200 OK                    -- every deployment has all pods ready (or there are none)
//   - 503 Service Unavailable   -- one or more deployments are degraded
//   - 500 Internal Server Error -- the Kubernetes API could not be reached
func deploymentsHealthHandler(clientset kubernetes.Interface) http.HandlerFunc {
	// The function we return is the actual HTTP handler.
	// Go closures "capture" variables from the surrounding scope,
	// so this inner function can use `clientset` even after deploymentsHealthHandler returns.
	return func(w http.ResponseWriter, r *http.Request) {
		// r.Context() is the context tied to this specific HTTP request.
		// If the client drops the connection, this context is cancelled automatically.
		report, err := getDeploymentsHealth(r.Context(), clientset)
		if err != nil {
			// http.Error writes a plain-text error body and the given status code.
			http.Error(w, fmt.Sprintf("failed to retrieve deployments: %v", err), http.StatusInternalServerError)
			return // stop processing this request
		}

		// Determine the HTTP status code based on cluster health.
		// We compute this once here and reuse it across all three format branches.
		statusCode := http.StatusOK // 200
		if !report.AllHealthy {
			statusCode = http.StatusServiceUnavailable // 503
		}

		// r.URL.Query().Get("format") reads the ?format= query parameter from the URL.
		// For example: /deployments/health?format=html  -> "html"
		//              /deployments/health?format=table -> "table"
		//              /deployments/health              -> ""
		switch r.URL.Query().Get("format") {
		case "html":
			// Card + badge dashboard view.
			renderHTML(w, report, statusCode)
		case "table":
			// Excel-style spreadsheet grid view.
			renderTable(w, report, statusCode)
		default:
			// Machine-readable JSON -- the default when no format is specified.
			renderJSON(w, report, statusCode)
		}
	}
}

// renderJSON writes the deployment health report as a JSON response.
// It is called when the request does NOT include ?format=html.
func renderJSON(w http.ResponseWriter, report *DeploymentsHealthReport, statusCode int) {
	// Tell the client the response body is JSON.
	w.Header().Set("Content-Type", "application/json")

	// Write the HTTP status code. Headers must be written before the body.
	w.WriteHeader(statusCode)

	// Encode the report struct as JSON and stream it directly to the response writer.
	// json.NewEncoder(w).Encode() is more efficient than json.Marshal + w.Write
	// because it avoids an intermediate in-memory buffer.
	if err := json.NewEncoder(w).Encode(report); err != nil {
		fmt.Println("failed writing JSON response")
	}
}

// htmlTemplateData is the data we pass into the HTML template.
// html/template fills in the {{ .Field }} placeholders using this struct.
// We compute the summary counts here so the template stays simple —
// templates should display data, not calculate it.
type htmlTemplateData struct {
	// Deployments is the full list of deployment statuses to render as table rows.
	Deployments []DeploymentStatus

	// Total is the total number of deployments across all namespaces.
	Total int

	// HealthyCount is the number of deployments with all replicas ready.
	HealthyCount int

	// DegradedCount is the number of deployments that are not fully ready.
	DegradedCount int
}

// htmlTemplate is the HTML page template. It is defined as a raw string literal
// (backtick-quoted) so we don't need a separate file.
//
// html/template syntax:
//   {{ .Field }}         inserts the value of Field from the data struct (auto-escaped)
//   {{ range .Slice }}   loops over a slice; inside the loop, . refers to the current element
//   {{ if .Bool }}       conditional block
//   {{ end }}            closes a range or if block
var htmlTemplate = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>Deployment Health</title>

	<!--
		The <meta http-equiv="refresh"> tag makes the browser reload the page
		automatically every 10 seconds. This is the simplest way to keep the
		dashboard up to date without writing any JavaScript.
	-->
	<meta http-equiv="refresh" content="10">

	<style>
		/* ── Reset & base ───────────────────────────────────────────────────── */
		*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

		body {
			font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
			/* Base font size increased to 16px for better legibility throughout. */
			font-size: 16px;
			/* Default text is pure black — overrides are applied only where colour is needed. */
			color: #000;
			background: #f5f5f5;
			padding: 32px;
		}

		/* ── Page title ─────────────────────────────────────────────────────── */
		h1 {
			font-size: 26px;
			font-weight: 600;
			margin-bottom: 24px;
			/* Title is black — no override needed, inherits from body. */
			color: #000;
		}

		/* ── Summary cards ──────────────────────────────────────────────────── */

		/* The three cards sit side by side using flexbox. */
		.cards {
			display: flex;
			gap: 16px;
			margin-bottom: 28px;
		}

		.card {
			background: #fff;
			border: 1px solid #e0e0e0;
			border-radius: 6px;
			padding: 20px 28px;
			min-width: 120px;
		}

		/* The small uppercase label above the number — kept black, slightly larger than before. */
		.card-label {
			font-size: 13px;
			font-weight: 600;
			letter-spacing: 0.08em;
			text-transform: uppercase;
			/* Black instead of the previous grey #888. */
			color: #000;
			margin-bottom: 8px;
		}

		/* The large number in each card. */
		.card-value {
			font-size: 36px;
			font-weight: 700;
			line-height: 1;
			/* Default card number (Total) is black. */
			color: #000;
		}

		/* The Healthy count stays green and the Degraded count stays orange —
		   these are the only two numbers that are NOT black, by design. */
		.card-value.healthy  { color: #2d9e5f; }
		.card-value.degraded { color: #d97706; }

		/* ── Data table ─────────────────────────────────────────────────────── */
		.table-wrapper {
			background: #fff;
			border: 1px solid #e0e0e0;
			border-radius: 6px;
			overflow: hidden; /* clips the rounded corners around the table */
		}

		table {
			width: 100%;
			border-collapse: collapse; /* removes double borders between cells */
		}

		/* Header row — labels are black, slightly larger than before. */
		thead th {
			padding: 14px 20px;
			text-align: left;
			font-size: 13px;
			font-weight: 700;
			letter-spacing: 0.07em;
			text-transform: uppercase;
			/* Black instead of the previous grey #888. */
			color: #000;
			border-bottom: 1px solid #e0e0e0;
		}

		/* Right-align the numeric columns. */
		thead th.num,
		tbody td.num {
			text-align: right;
		}

		/* Body rows — increased padding and font size for better readability.
		   All cell text is black. */
		tbody td {
			padding: 16px 20px;
			font-size: 16px;
			border-bottom: 1px solid #f0f0f0;
			/* Black instead of the previous dark grey #333. */
			color: #000;
		}

		/* Remove the border from the very last row. */
		tbody tr:last-child td { border-bottom: none; }

		/* Subtle highlight when hovering a row. */
		tbody tr:hover { background: #fafafa; }

		/* Orange text for the ready count when a deployment is degraded.
		   This is the only table cell that is NOT black — it signals a problem. */
		.ready-degraded { color: #d97706; font-weight: 600; }

		/* ── Status badges ──────────────────────────────────────────────────── */

		/* Base badge style — a small pill shape.
		   Font size is unchanged from the original design as requested. */
		.badge {
			display: inline-flex;
			align-items: center;
			gap: 6px;
			padding: 4px 12px;
			border-radius: 999px; /* fully rounded ends */
			font-size: 13px;
			font-weight: 500;
		}

		/* The coloured dot before the label text. */
		.badge::before {
			content: "●";
			font-size: 8px;
		}

		/* Green badge for healthy deployments — colour kept as-is. */
		.badge.healthy {
			background: #dcfce7;
			color: #16a34a;
		}

		/* Orange badge for degraded deployments — colour kept as-is. */
		.badge.degraded {
			background: #fef3c7;
			color: #d97706;
		}

		/* ── Footer ─────────────────────────────────────────────────────────── */
		.footer {
			margin-top: 16px;
			font-size: 16px;
			font-weight: 500;
			color: #000;
		}

		/* Links in the footer (JSON / Table). */
		.footer a {
			color: #000;
			font-weight: 600;
			text-decoration: none;
		}
		.footer a:hover { text-decoration: underline; }
	</style>
</head>
<body>

<h1>Deployment Health</h1>

<!-- Summary cards -->
<div class="cards">
	<div class="card">
		<div class="card-label">Total</div>
		<div class="card-value">{{ .Total }}</div>
	</div>
	<div class="card">
		<div class="card-label">Healthy</div>
		<div class="card-value healthy">{{ .HealthyCount }}</div>
	</div>
	<div class="card">
		<div class="card-label">Degraded</div>
		<div class="card-value degraded">{{ .DegradedCount }}</div>
	</div>
</div>

<!-- Deployments table -->
<div class="table-wrapper">
	<table>
		<thead>
			<tr>
				<th>Namespace</th>
				<th>Name</th>
				<th class="num">Desired</th>
				<th class="num">Ready</th>
				<th>Status</th>
			</tr>
		</thead>
		<tbody>
			{{ range .Deployments }}
			<tr>
				<td>{{ .Namespace }}</td>
				<td>{{ .Name }}</td>
				<td class="num">{{ .DesiredReplicas }}</td>
				{{ if .Healthy }}
				<td class="num">{{ .ReadyReplicas }}</td>
				<td><span class="badge healthy">Healthy</span></td>
				{{ else }}
				<td class="num ready-degraded">{{ .ReadyReplicas }}</td>
				<td><span class="badge degraded">Degraded</span></td>
				{{ end }}
			</tr>
			{{ end }}
		</tbody>
	</table>
</div>

<!-- Footer with refresh notice and links to switch between views.
     The current view (HTML) is shown as plain text, not a link. -->
<p class="footer">
	Auto-refreshes every 10s &mdash;
	<a href="/deployments/health">JSON</a> &middot;
	HTML &middot;
	<a href="/deployments/health?format=table">Table</a>
</p>

</body>
</html>
`))

// renderHTML writes the deployment health report as a self-refreshing HTML page.
// It is called when the request includes ?format=html.
//
// Steps:
//  1. Count healthy and degraded deployments for the summary cards.
//  2. Build the htmlTemplateData struct with those counts.
//  3. Write the HTTP status code and Content-Type header.
//  4. Execute the HTML template, which fills in the placeholders and writes
//     the finished HTML directly to the response writer.
func renderHTML(w http.ResponseWriter, report *DeploymentsHealthReport, statusCode int) {
	// Count how many deployments are healthy so we can show it in the summary card.
	healthyCount := 0
	for _, d := range report.Deployments {
		if d.Healthy {
			healthyCount++
		}
	}

	// Build the data object the template will use.
	// DegradedCount is derived from Total minus HealthyCount.
	data := htmlTemplateData{
		Deployments:   report.Deployments,
		Total:         len(report.Deployments),
		HealthyCount:  healthyCount,
		DegradedCount: len(report.Deployments) - healthyCount,
	}

	// Tell the browser this is an HTML page, not plain text or JSON.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Write the HTTP status code before writing the body.
	w.WriteHeader(statusCode)

	// Execute the template: fill in all {{ }} placeholders using `data`
	// and write the resulting HTML directly to the response writer w.
	// If rendering fails (e.g. a bug in the template), log it — we can't
	// send a new status code at this point because WriteHeader was already called.
	if err := htmlTemplate.Execute(w, data); err != nil {
		fmt.Printf("failed rendering HTML template: %v\n", err)
	}
}

// tableTemplate is the Excel-style spreadsheet view, served at ?format=table.
//
// Key visual differences from the HTML dashboard:
//   - Every cell has a border on all four sides — mimicking a spreadsheet grid
//   - The header row has a grey background, like a frozen header row in Excel
//   - Rows alternate between white and very light grey for readability
//   - Numeric columns use a monospace font so digits line up vertically
//   - The STATUS column shows plain text (Healthy / Degraded) instead of pill badges
//   - The footer shows "Table" as plain text (current view) and links to JSON and HTML
var tableTemplate = template.Must(template.New("spreadsheet").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>Deployment Health</title>
	<meta http-equiv="refresh" content="10">
	<style>
		/* ── Reset & base ───────────────────────────────────────────────────── */
		*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

		body {
			font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
			font-size: 16px;
			color: #000;
			background: #f5f5f5;
			padding: 32px;
		}

		/* ── Page title ─────────────────────────────────────────────────────── */
		h1 {
			font-size: 26px;
			font-weight: 600;
			margin-bottom: 24px;
			color: #000;
		}

		/* ── Summary cards — identical to the HTML dashboard ────────────────── */
		.cards {
			display: flex;
			gap: 16px;
			margin-bottom: 28px;
		}

		.card {
			background: #fff;
			border: 1px solid #e0e0e0;
			border-radius: 6px;
			padding: 20px 28px;
			min-width: 120px;
		}

		.card-label {
			font-size: 13px;
			font-weight: 600;
			letter-spacing: 0.08em;
			text-transform: uppercase;
			color: #000;
			margin-bottom: 8px;
		}

		.card-value        { font-size: 36px; font-weight: 700; line-height: 1; color: #000; }
		.card-value.healthy  { color: #2d9e5f; }
		.card-value.degraded { color: #d97706; }

		/* ── Spreadsheet table ──────────────────────────────────────────────── */

		/* The outer wrapper has no rounded corners — spreadsheets are flat and boxy. */
		.table-wrapper {
			background: #fff;
			border: 2px solid #bbb; /* slightly thicker outer border, like a sheet boundary */
		}

		table {
			width: 100%;
			/* collapse: adjacent cell borders merge into one line, giving the grid look. */
			border-collapse: collapse;
		}

		/* Header row — grey background with bold black text, like a frozen Excel header. */
		thead th {
			padding: 12px 16px;
			text-align: left;
			font-size: 13px;
			font-weight: 700;
			letter-spacing: 0.07em;
			text-transform: uppercase;
			color: #000;
			background: #e8e8e8; /* light grey — the classic Excel column header colour */
			/* Every edge of every header cell gets a border. */
			border: 1px solid #bbb;
		}

		/* Right-align the numeric columns — digits line up like in a spreadsheet. */
		thead th.num,
		tbody td.num {
			text-align: right;
		}

		/* Body cells — full border on all four sides creates the grid. */
		tbody td {
			padding: 12px 16px;
			font-size: 16px;
			color: #000;
			border: 1px solid #d0d0d0; /* slightly lighter than the header borders */
		}

		/* Monospace font for the DESIRED and READY columns so numbers align vertically,
		   exactly like a spreadsheet's number cells. */
		tbody td.num {
			font-family: "Courier New", Consolas, monospace;
			font-size: 15px;
		}

		/* Alternating row shading — every even row gets a very pale grey background.
		   This is the most recognisable visual pattern of a spreadsheet. */
		tbody tr:nth-child(even) { background: #f7f7f7; }
		tbody tr:nth-child(odd)  { background: #ffffff; }

		/* Subtle highlight on hover so the user can track which row they are on. */
		tbody tr:hover { background: #eef4ff; }

		/* Orange text for the READY count when a deployment is degraded. */
		.ready-degraded {
			color: #d97706;
			font-weight: 700;
		}

		/* Plain-text status — no pill badge, just coloured text like a cell value. */
		.status-healthy  { color: #16a34a; font-weight: 600; }
		.status-degraded { color: #d97706; font-weight: 700; }

		/* ── Footer ─────────────────────────────────────────────────────────── */
		.footer {
			margin-top: 16px;
			font-size: 16px;
			font-weight: 500;
			color: #000;
		}

		.footer a          { color: #000; font-weight: 600; text-decoration: none; }
		.footer a:hover    { text-decoration: underline; }

		/* The active view label is bold and not a link. */
		.footer .active    { font-weight: 700; }
	</style>
</head>
<body>

<h1>Deployment Health</h1>

<!-- Summary cards — same as the HTML dashboard -->
<div class="cards">
	<div class="card">
		<div class="card-label">Total</div>
		<div class="card-value">{{ .Total }}</div>
	</div>
	<div class="card">
		<div class="card-label">Healthy</div>
		<div class="card-value healthy">{{ .HealthyCount }}</div>
	</div>
	<div class="card">
		<div class="card-label">Degraded</div>
		<div class="card-value degraded">{{ .DegradedCount }}</div>
	</div>
</div>

<!-- Spreadsheet table -->
<div class="table-wrapper">
	<table>
		<thead>
			<tr>
				<th>Namespace</th>
				<th>Name</th>
				<th class="num">Desired</th>
				<th class="num">Ready</th>
				<th>Status</th>
			</tr>
		</thead>
		<tbody>
			{{ range .Deployments }}
			<tr>
				<td>{{ .Namespace }}</td>
				<td>{{ .Name }}</td>
				<td class="num">{{ .DesiredReplicas }}</td>
				{{ if .Healthy }}
				<td class="num">{{ .ReadyReplicas }}</td>
				<td class="status-healthy">Healthy</td>
				{{ else }}
				<td class="num ready-degraded">{{ .ReadyReplicas }}</td>
				<td class="status-degraded">Degraded</td>
				{{ end }}
			</tr>
			{{ end }}
		</tbody>
	</table>
</div>

<!-- Footer — "Table" is the current active view so it is plain text, not a link. -->
<p class="footer">
	Auto-refreshes every 10s &mdash;
	<a href="/deployments/health">JSON</a> &middot;
	<a href="/deployments/health?format=html">HTML</a> &middot;
	<span class="active">Table</span>
</p>

</body>
</html>
`))

// renderTable writes the deployment health report as an Excel-style HTML table.
// It is called when the request includes ?format=table.
//
// It reuses the same htmlTemplateData struct as renderHTML because both views
// need exactly the same data: the deployment list plus the three summary counts.
func renderTable(w http.ResponseWriter, report *DeploymentsHealthReport, statusCode int) {
	// Count healthy deployments for the summary cards — same logic as renderHTML.
	healthyCount := 0
	for _, d := range report.Deployments {
		if d.Healthy {
			healthyCount++
		}
	}

	// Build the template data struct — identical shape to the HTML dashboard data.
	data := htmlTemplateData{
		Deployments:   report.Deployments,
		Total:         len(report.Deployments),
		HealthyCount:  healthyCount,
		DegradedCount: len(report.Deployments) - healthyCount,
	}

	// Tell the browser this is an HTML page.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Write the status code before the body — HTTP requires headers first.
	w.WriteHeader(statusCode)

	// Render the spreadsheet template into the response writer.
	if err := tableTemplate.Execute(w, data); err != nil {
		fmt.Printf("failed rendering table template: %v\n", err)
	}
}
