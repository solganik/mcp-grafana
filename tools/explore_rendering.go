package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/mark3labs/mcp-go/mcp"

	mcpgrafana "github.com/grafana/mcp-grafana"
)

const (
	defaultExploreWidth     = 1200
	defaultExploreHeight    = 600
	defaultExploreScale     = 1
	defaultExploreCrop      = "exploreVisualization"
	maxExploreWidth         = 4000
	maxExploreHeight        = 4000
	maxExploreTimeout       = 300
	maxExploreQueries       = 8
	maxExploreVariables     = 20
	maxExploreVariableKey   = 128
	maxExploreVariableValue = 256
	maxExploreModelBytes    = 32 * 1024
	maxExploreModelFields   = 256
	maxExploreURLLength     = 100 * 1024
	maxExploreStringLength  = 1024
)

var browserRenderMu sync.Mutex

type ExploreRenderQuery struct {
	RefID string         `json:"refId,omitempty" jsonschema:"description=Reference ID for the query. Defaults to A\\, B\\, C and so on."`
	Model map[string]any `json:"model" jsonschema:"required,description=Datasource-specific Grafana query model. For Prometheus this includes expr\\, format\\, instant\\, and range."`
}

type ExploreRenderParams struct {
	DatasourceUID  string               `json:"datasourceUid" jsonschema:"required,description=UID of the Grafana datasource"`
	DatasourceType string               `json:"datasourceType,omitempty" jsonschema:"description=Grafana datasource type. If omitted\\, it is resolved from datasourceUid."`
	Queries        []ExploreRenderQuery `json:"queries" jsonschema:"required,description=Datasource-specific Explore query models"`
	TimeRange      RenderTimeRange      `json:"timeRange" jsonschema:"required,description=Explore time range using relative values or epoch milliseconds"`
	Variables      map[string]string    `json:"variables,omitempty" jsonschema:"description=Optional Explore URL variables"`
	Theme          string               `json:"theme,omitempty" jsonschema:"description=Theme for the rendered image: light or dark. Defaults to dark"`
	Width          int                  `json:"width,omitempty" jsonschema:"description=Viewport width in pixels. Defaults to 1200"`
	Height         int                  `json:"height,omitempty" jsonschema:"description=Viewport height in pixels. Defaults to 600"`
	Scale          int                  `json:"scale,omitempty" jsonschema:"description=Device scale factor from 1 to 3. Defaults to 1"`
	TimeoutSeconds int                  `json:"timeoutSeconds,omitempty" jsonschema:"description=Rendering timeout in seconds from 1 to 300. Defaults to 60"`
	OutputPath     string               `json:"outputPath" jsonschema:"required,description=Absolute PNG path below the configured artifact output root"`
	Crop           string               `json:"crop,omitempty" jsonschema:"description=Capture mode: exploreVisualization or viewport. Defaults to exploreVisualization"`
}

type ExploreRenderResult struct {
	Path       string `json:"path"`
	MIMEType   string `json:"mimeType"`
	Bytes      int    `json:"bytes"`
	SHA256     string `json:"sha256"`
	ExploreURL string `json:"exploreUrl"`
	RenderMode string `json:"renderMode"`
}

type explorePane struct {
	Datasource string           `json:"datasource"`
	Queries    []map[string]any `json:"queries"`
	Range      RenderTimeRange  `json:"range"`
}

func renderExploreImage(ctx context.Context, args ExploreRenderParams) (ExploreRenderResult, error) {
	config := mcpgrafana.GrafanaConfigFromContext(ctx)
	baseURL := strings.TrimRight(config.URL, "/")
	if baseURL == "" {
		return ExploreRenderResult{}, fmt.Errorf("grafana URL not configured. Please set GRAFANA_URL environment variable")
	}

	options, err := validateExploreRenderParams(args)
	if err != nil {
		return ExploreRenderResult{}, err
	}
	outputPath, err := validateArtifactPath(config.ArtifactOutputRoot, args.OutputPath)
	if err != nil {
		return ExploreRenderResult{}, err
	}
	datasourceType := args.DatasourceType
	if datasourceType == "" {
		if mcpgrafana.GrafanaClientFromContext(ctx) == nil {
			return ExploreRenderResult{}, fmt.Errorf("datasourceType is required when no Grafana client is configured")
		}
		ds, err := getDatasourceByUID(ctx, GetDatasourceByUIDParams{UID: args.DatasourceUID})
		if err != nil {
			return ExploreRenderResult{}, fmt.Errorf("resolve datasource type: %w", err)
		}
		if ds == nil || ds.Type == "" {
			return ExploreRenderResult{}, fmt.Errorf("datasource %q has no type", args.DatasourceUID)
		}
		datasourceType = ds.Type
	}

	exploreURL, err := buildExploreRenderURL(baseURL, config.OrgID, datasourceType, args)
	if err != nil {
		return ExploreRenderResult{}, fmt.Errorf("build Explore URL: %w", err)
	}

	cookie, err := loadSessionCookie(baseURL, config.BrowserAuth)
	if err != nil {
		return ExploreRenderResult{}, err
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return ExploreRenderResult{}, fmt.Errorf("invalid Grafana URL: %w", err)
	}

	imageData, err := renderWithChromeActions(ctx, exploreURL, cookie, parsedURL.Hostname(),
		options.width, options.height, options.scale, options.timeout,
		runExploreQuery(),
		waitForExploreReady(),
		captureExploreScreenshot(options.crop),
	)
	if err != nil {
		return ExploreRenderResult{}, fmt.Errorf("Explore rendering failed: %w", err)
	}
	if len(imageData) == 0 {
		return ExploreRenderResult{}, fmt.Errorf("Explore rendering produced an empty image")
	}

	if err := writeArtifactAtomically(outputPath, imageData); err != nil {
		return ExploreRenderResult{}, fmt.Errorf("write rendered artifact: %w", err)
	}
	sum := sha256.Sum256(imageData)
	return ExploreRenderResult{
		Path:       outputPath,
		MIMEType:   "image/png",
		Bytes:      len(imageData),
		SHA256:     hex.EncodeToString(sum[:]),
		ExploreURL: exploreURL,
		RenderMode: "browser-explore",
	}, nil
}

type exploreRenderOptions struct {
	width, height, scale int
	timeout              time.Duration
	crop                 string
}

func validateExploreRenderParams(args ExploreRenderParams) (exploreRenderOptions, error) {
	if strings.TrimSpace(args.DatasourceUID) == "" {
		return exploreRenderOptions{}, fmt.Errorf("datasourceUid is required")
	}
	if len(args.DatasourceUID) > maxExploreStringLength || strings.ContainsAny(args.DatasourceUID, "\r\n") {
		return exploreRenderOptions{}, fmt.Errorf("datasourceUid must be at most %d bytes and contain no newlines", maxExploreStringLength)
	}
	if len(args.Queries) == 0 || len(args.Queries) > maxExploreQueries {
		return exploreRenderOptions{}, fmt.Errorf("queries must contain between 1 and %d items", maxExploreQueries)
	}
	if args.TimeRange.From == "" || args.TimeRange.To == "" {
		return exploreRenderOptions{}, fmt.Errorf("timeRange.from and timeRange.to are required")
	}
	if len(args.TimeRange.From) > maxExploreStringLength || len(args.TimeRange.To) > maxExploreStringLength ||
		strings.ContainsAny(args.TimeRange.From+args.TimeRange.To, "\r\n") {
		return exploreRenderOptions{}, fmt.Errorf("time range values must be at most %d bytes and contain no newlines", maxExploreStringLength)
	}
	if args.OutputPath == "" {
		return exploreRenderOptions{}, fmt.Errorf("outputPath is required")
	}
	theme := args.Theme
	if theme == "" {
		theme = "dark"
	}
	if theme != "dark" && theme != "light" {
		return exploreRenderOptions{}, fmt.Errorf("theme must be light or dark")
	}
	width := args.Width
	if width == 0 {
		width = defaultExploreWidth
	}
	height := args.Height
	if height == 0 {
		height = defaultExploreHeight
	}
	scale := args.Scale
	if scale == 0 {
		scale = defaultExploreScale
	}
	timeout := args.TimeoutSeconds
	if timeout == 0 {
		timeout = int(defaultRenderTimeout / time.Second)
	}
	if width < 200 || width > maxExploreWidth || height < 100 || height > maxExploreHeight {
		return exploreRenderOptions{}, fmt.Errorf("width must be 200-%d and height must be 100-%d", maxExploreWidth, maxExploreHeight)
	}
	if scale < 1 || scale > 3 {
		return exploreRenderOptions{}, fmt.Errorf("scale must be 1-3")
	}
	if timeout < 1 || timeout > maxExploreTimeout {
		return exploreRenderOptions{}, fmt.Errorf("timeoutSeconds must be 1-%d", maxExploreTimeout)
	}
	crop := args.Crop
	if crop == "" {
		crop = defaultExploreCrop
	}
	if crop != "exploreVisualization" && crop != "viewport" {
		return exploreRenderOptions{}, fmt.Errorf("crop must be exploreVisualization or viewport")
	}
	if len(args.Variables) > maxExploreVariables {
		return exploreRenderOptions{}, fmt.Errorf("variables cannot contain more than %d entries", maxExploreVariables)
	}
	for key, value := range args.Variables {
		if strings.TrimSpace(key) == "" || len(key) > maxExploreVariableKey || len(value) > maxExploreVariableValue ||
			strings.ContainsAny(key+value, "\r\n") {
			return exploreRenderOptions{}, fmt.Errorf("Explore variable keys must be at most %d bytes and values at most %d bytes", maxExploreVariableKey, maxExploreVariableValue)
		}
		switch key {
		case "panes", "schemaVersion", "orgId", "theme":
			return exploreRenderOptions{}, fmt.Errorf("Explore variable %q is reserved", key)
		}
	}
	for i, query := range args.Queries {
		if query.Model == nil {
			return exploreRenderOptions{}, fmt.Errorf("queries[%d].model is required", i)
		}
		modelBytes, err := json.Marshal(query.Model)
		if err != nil {
			return exploreRenderOptions{}, fmt.Errorf("marshal queries[%d].model: %w", i, err)
		}
		if len(modelBytes) > maxExploreModelBytes {
			return exploreRenderOptions{}, fmt.Errorf("queries[%d].model exceeds %d bytes", i, maxExploreModelBytes)
		}
		if len(query.Model) > maxExploreModelFields {
			return exploreRenderOptions{}, fmt.Errorf("queries[%d].model contains more than %d fields", i, maxExploreModelFields)
		}
		if query.RefID != "" && (len(query.RefID) > 16 || strings.ContainsAny(query.RefID, "\r\n")) {
			return exploreRenderOptions{}, fmt.Errorf("queries[%d].refId is invalid", i)
		}
	}
	return exploreRenderOptions{
		width: width, height: height, scale: scale,
		timeout: time.Duration(timeout) * time.Second,
		crop:    crop,
	}, nil
}

func buildExploreRenderURL(baseURL string, orgID int64, datasourceType string, args ExploreRenderParams) (string, error) {
	if strings.TrimSpace(datasourceType) == "" {
		return "", fmt.Errorf("datasourceType is required")
	}
	if len(datasourceType) > maxExploreStringLength || strings.ContainsAny(datasourceType, "\r\n") {
		return "", fmt.Errorf("datasourceType must be at most %d bytes and contain no newlines", maxExploreStringLength)
	}
	parsedBase, err := url.Parse(baseURL)
	if err != nil || parsedBase.Scheme == "" || parsedBase.Host == "" {
		return "", fmt.Errorf("base Grafana URL must be absolute")
	}
	queries := make([]map[string]any, 0, len(args.Queries))
	for i, query := range args.Queries {
		model := make(map[string]any, len(query.Model)+1)
		for key, value := range query.Model {
			model[key] = value
		}
		if _, ok := model["datasource"]; !ok {
			model["datasource"] = map[string]string{
				"uid":  args.DatasourceUID,
				"type": datasourceType,
			}
		}
		if _, ok := model["editorMode"]; !ok && isExpressionDatasource(datasourceType) {
			if expression, ok := model["expr"].(string); ok && strings.TrimSpace(expression) != "" {
				model["editorMode"] = "code"
			}
		}
		refID := query.RefID
		if refID == "" {
			refID = string(rune('A' + i))
		}
		model["refId"] = refID
		queries = append(queries, model)
	}
	pane := explorePane{
		Datasource: args.DatasourceUID,
		Queries:    queries,
		Range:      args.TimeRange,
	}
	panesJSON, err := json.Marshal(map[string]explorePane{"abc": pane})
	if err != nil {
		return "", fmt.Errorf("marshal panes: %w", err)
	}
	leftJSON, err := json.Marshal(pane)
	if err != nil {
		return "", fmt.Errorf("marshal Explore left state: %w", err)
	}
	params := url.Values{}
	params.Set("panes", string(panesJSON))
	params.Set("left", string(leftJSON))
	params.Set("schemaVersion", "1")
	if orgID <= 0 {
		orgID = 1
	}
	params.Set("orgId", strconv.FormatInt(orgID, 10))
	theme := args.Theme
	if theme == "" {
		theme = "dark"
	}
	params.Set("theme", theme)
	for key, value := range args.Variables {
		params.Set(key, value)
	}
	result := strings.TrimRight(baseURL, "/") + "/explore?" + params.Encode()
	if len(result) > maxExploreURLLength {
		return "", fmt.Errorf("Explore URL exceeds %d bytes", maxExploreURLLength)
	}
	return result, nil
}

func isExpressionDatasource(datasourceType string) bool {
	switch strings.ToLower(datasourceType) {
	case "prometheus", "loki":
		return true
	default:
		return false
	}
}

type exploreReadyState struct {
	Ready bool   `json:"ready"`
	Error string `json:"error"`
}

func runExploreQuery() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		const script = `(() => {
			const selectors = [
				'[data-testid*="RefreshPicker run button"]',
				'button[aria-label="Run query"]'
			];
			for (const selector of selectors) {
				const button = document.querySelector(selector);
				if (!button) continue;
				const rect = button.getBoundingClientRect();
				if (rect.width <= 0 || rect.height <= 0) continue;
				const disabled = button.disabled ||
					button.getAttribute("aria-disabled") === "true" ||
					button.getAttribute("data-testid")?.includes("disabled");
				const hasQueryEditor = document.querySelector(
					".query-editor-row, [data-testid*='query editor'], textarea, input"
				) !== null;
				if (!disabled && hasQueryEditor) return selector;
			}
			return "";
		})()`
		pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		var selector string
		var readySince time.Time
		for {
			if err := chromedp.Evaluate(script, &selector).Do(ctx); err != nil {
				return err
			}
			if selector != "" {
				if readySince.IsZero() {
					readySince = time.Now()
				}
				if time.Since(readySince) >= time.Second {
					break
				}
			} else {
				readySince = time.Time{}
			}
			select {
			case <-pollCtx.Done():
				return fmt.Errorf("Explore query controls did not finish initializing: %w", pollCtx.Err())
			case <-ticker.C:
			}
		}

		requests := make(chan string, 1)
		chromedp.ListenTarget(ctx, func(event any) {
			request, ok := event.(*network.EventRequestWillBeSent)
			if !ok || !strings.Contains(request.Request.URL, "/api/ds/query") {
				return
			}
			select {
			case requests <- request.Request.URL:
			default:
			}
		})
		clickScript := fmt.Sprintf(`(() => {
			const button = document.querySelector(%q);
			if (!button) return false;
			button.click();
			return true;
		})()`, selector)
		var clicked bool
		if err := chromedp.Evaluate(clickScript, &clicked).Do(ctx); err != nil {
			return fmt.Errorf("click Explore Run query button: %w", err)
		}
		if !clicked {
			return fmt.Errorf("Explore Run query button disappeared before it could be clicked")
		}
		requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
		defer requestCancel()
		select {
		case <-requests:
			return nil
		case <-requestCtx.Done():
			const diagnosticScript = `(() => {
				const button = document.querySelector('[data-testid="RefreshPicker run button"], button[aria-label="Run query"]');
				return {
					path: window.location.pathname,
					buttonText: button?.textContent?.trim() || "",
					buttonDisabled: Boolean(button?.disabled) || button?.getAttribute("aria-disabled") === "true",
					buttonHTML: button?.outerHTML?.slice(0, 500) || "",
					queryEditors: document.querySelectorAll(".query-editor-row, [data-testid*='query editor']").length,
					bodyText: document.body?.innerText?.trim().slice(0, 300) || ""
				};
			})()`
			var diagnostic map[string]any
			if err := chromedp.Evaluate(diagnosticScript, &diagnostic).Do(ctx); err == nil {
				encoded, marshalErr := json.Marshal(diagnostic)
				if marshalErr == nil {
					return fmt.Errorf("Explore Run query did not emit a /api/ds/query request: %w (browser state: %s)", requestCtx.Err(), encoded)
				}
			}
			return fmt.Errorf("Explore Run query did not emit a /api/ds/query request: %w", requestCtx.Err())
		}
	})
}

func waitForExploreReady() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		const script = `(() => {
			const visible = (element) => {
				if (!element) return false;
				const rect = element.getBoundingClientRect();
				return rect.width > 0 && rect.height > 0 &&
					getComputedStyle(element).visibility !== "hidden";
			};
			const firstVisible = (selector) => [...document.querySelectorAll(selector)]
				.find((element) => visible(element));
			const currentPath = window.location.pathname.toLowerCase();
			if (currentPath === "/login" || currentPath.endsWith("/login") ||
				document.body?.innerText?.toLowerCase().includes("sign in to grafana")) {
				return {ready: false, error: "Grafana authentication is required"};
			}
			const errors = [
				"[data-testid*='error']",
				".alert-error",
				".query-editor-row .alert",
				"[role='alert']"
			];
			for (const selector of errors) {
				const element = document.querySelector(selector);
				if (visible(element) && element.textContent.trim()) {
					return {ready: false, error: element.textContent.trim().slice(0, 500)};
				}
			}
			const loading = [
				".panel-loading",
				"[aria-label='Loading']",
				"[data-testid*='loading']"
			].some((selector) => visible(document.querySelector(selector)));
			const noData = [
				".no-data",
				"[data-testid*='no-data']",
				".panel-no-data"
			].some((selector) => visible(document.querySelector(selector))) ||
				[...document.querySelectorAll(".explore-container *")].some((element) =>
					visible(element) && /^(no data|no datapoints)$/i.test(element.textContent.trim()));
			const visualizations = [
				".explore-container .panel-content",
				".panel-content",
				".explore-container [data-testid*='panel content']",
				"[data-testid*='panel content']",
				"[data-testid*='visualization']",
				".explore-container canvas",
				".explore-container svg",
				"canvas",
				"svg"
			];
			if (noData) return {ready: false, error: "Explore query returned no data"};
			const visualization = visualizations
				.map((selector) => firstVisible(selector))
				.find(Boolean);
			const ready = !loading && Boolean(visualization) &&
				(visualization.tagName !== "CANVAS" ||
					(visualization.width > 0 && visualization.height > 0)) &&
				(visualization.tagName === "CANVAS" ||
					visualization.childElementCount > 0 ||
					visualization.textContent.trim().length > 0);
			return {ready, error: ""};
		})()`
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			var state exploreReadyState
			if err := chromedp.Evaluate(script, &state).Do(ctx); err != nil {
				return err
			}
			if state.Error != "" {
				return fmt.Errorf("Grafana Explore query error: %s", state.Error)
			}
			if state.Ready {
				return nil
			}
			select {
			case <-ctx.Done():
				const diagnosticScript = `(() => ({
					bodyText: document.body?.innerText?.trim().slice(-500) || "",
					panelContents: document.querySelectorAll(".panel-content").length,
					canvases: document.querySelectorAll("canvas").length,
					svgs: document.querySelectorAll("svg").length,
					alerts: [...document.querySelectorAll("[role='alert'], .alert-error")].map((e) => e.textContent.trim()).filter(Boolean).slice(0, 3)
				}))()`
				var diagnostic map[string]any
				if err := chromedp.Evaluate(diagnosticScript, &diagnostic).Do(ctx); err == nil {
					encoded, marshalErr := json.Marshal(diagnostic)
					if marshalErr == nil {
						return fmt.Errorf("timeout waiting for Explore visualization: %w (browser state: %s)", ctx.Err(), encoded)
					}
				}
				return fmt.Errorf("timeout waiting for Explore visualization: %w", ctx.Err())
			case <-ticker.C:
			}
		}
	})
}

type exploreClip struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type exploreClipMeasurement struct {
	Clip             exploreClip `json:"clip"`
	DocumentWidth    float64     `json:"documentWidth"`
	DocumentHeight   float64     `json:"documentHeight"`
	Reliable         bool        `json:"reliable"`
	SelectedSelector string      `json:"selectedSelector"`
}

func normalizeExploreClip(measurement exploreClipMeasurement) (exploreClip, bool) {
	clip := measurement.Clip
	if !measurement.Reliable || measurement.DocumentWidth <= 0 || measurement.DocumentHeight <= 0 {
		return exploreClip{}, false
	}
	if clip.Width <= 0 || clip.Height <= 0 {
		return exploreClip{}, false
	}
	clip.X = maxFloat(0, minFloat(clip.X, measurement.DocumentWidth))
	clip.Y = maxFloat(0, minFloat(clip.Y, measurement.DocumentHeight))
	clip.Width = minFloat(clip.Width, measurement.DocumentWidth-clip.X)
	clip.Height = minFloat(clip.Height, measurement.DocumentHeight-clip.Y)
	if clip.Width <= 0 || clip.Height <= 0 {
		return exploreClip{}, false
	}
	return clip, true
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func captureExploreScreenshot(crop string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var clip exploreClip
		if crop == "exploreVisualization" {
			const script = `(() => {
				const visible = (element) => {
					if (!element) return false;
					const rect = element.getBoundingClientRect();
					const style = getComputedStyle(element);
					return rect.width > 0 && rect.height > 0 &&
						style.display !== "none" && style.visibility !== "hidden";
				};
				const hasVisualization = (element) =>
					!!element?.querySelector("canvas, svg, [role='img'], [data-testid*='visualization']");
				const containsRenderedVisual = (element) => {
					const elementRect = element.getBoundingClientRect();
					const visual = [...element.querySelectorAll("canvas, svg, [role='img']")]
						.filter(visible);
					if (visual.length === 0) return false;
					const bounds = visual.reduce((result, child) => {
						const rect = child.getBoundingClientRect();
						return {
							left: Math.min(result.left, rect.left),
							top: Math.min(result.top, rect.top),
							right: Math.max(result.right, rect.right),
							bottom: Math.max(result.bottom, rect.bottom)
						};
					}, {left: Infinity, top: Infinity, right: -Infinity, bottom: -Infinity});
					// Reject a panel-content wrapper that clips an overflowing
					// canvas. Its ancestor may contain the axes/labels and is
					// the only safe element crop candidate.
					return elementRect.left <= bounds.left + 1 &&
						elementRect.top <= bounds.top + 1 &&
						elementRect.right >= bounds.right - 1 &&
						elementRect.bottom >= bounds.bottom - 1;
				};
				const candidates = [
					"[data-testid*='panel content']",
					".explore-container .panel-content",
					".panel-content",
					"[data-testid*='visualization']"
				];
				let selected = null;
				let selectedSelector = "";
				for (const selector of candidates) {
					const element = [...document.querySelectorAll(selector)]
						.find((candidate) => visible(candidate) && hasVisualization(candidate) &&
							containsRenderedVisual(candidate));
					if (element) {
						selected = element;
						selectedSelector = selector;
						break;
					}
				}
				// Some Grafana versions do not put a stable class on the panel
				// content. Walk from the rendered visual to the nearest useful
				// ancestor, while deliberately excluding the whole Explore shell.
				if (!selected) {
					const visual = [...document.querySelectorAll("canvas, svg, [role='img']")]
						.find((candidate) => visible(candidate));
					for (let element = visual?.parentElement; element && element !== document.body;
						element = element.parentElement) {
						if (element.classList.contains("explore-container")) continue;
						if (visible(element) && hasVisualization(element) &&
							containsRenderedVisual(element)) {
							selected = element;
							selectedSelector = "visualization ancestor";
							break;
						}
					}
				}
				if (!selected) {
					return {
						reliable: false,
						clip: {x: 0, y: 0, width: 0, height: 0},
						documentWidth: 0, documentHeight: 0, selectedSelector: ""
					};
				}
				selected.scrollIntoView({block: "center", inline: "nearest"});
				const rect = selected.getBoundingClientRect();
				const documentWidth = Math.max(
					document.documentElement.scrollWidth, document.body?.scrollWidth || 0,
					window.innerWidth);
				const documentHeight = Math.max(
					document.documentElement.scrollHeight, document.body?.scrollHeight || 0,
					window.innerHeight);
				const x = rect.left + window.scrollX;
				const y = rect.top + window.scrollY;
				const reliable = Number.isFinite(x) && Number.isFinite(y) &&
					Number.isFinite(rect.width) && Number.isFinite(rect.height) &&
					rect.width >= 100 && rect.height >= 50 &&
					rect.width <= documentWidth && rect.height <= documentHeight;
				return {
					reliable,
					clip: {x, y, width: rect.width, height: rect.height},
					documentWidth, documentHeight, selectedSelector
				};
			})()`
			var measurement exploreClipMeasurement
			if err := chromedp.Evaluate(script, &measurement).Do(ctx); err != nil {
				return err
			}
			// scrollIntoView can trigger a final layout pass. Re-read after two
			// frames so the clip matches the layout used by captureScreenshot.
			if err := chromedp.Sleep(100 * time.Millisecond).Do(ctx); err != nil {
				return err
			}
			// Recompute after the layout settle, because the first
			// getBoundingClientRect() may have been invalidated by scrolling.
			if err := chromedp.Evaluate(script, &measurement).Do(ctx); err != nil {
				return err
			}
			clip, _ = normalizeExploreClip(measurement)
			if clip.Width <= 0 || clip.Height <= 0 {
				// A clipped canvas is worse than a complete viewport image. Keep
				// the default mode safe when Grafana's DOM changes.
				crop = "viewport"
			}
		}
		var image []byte
		capture := page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithCaptureBeyondViewport(crop == "exploreVisualization")
		if crop == "exploreVisualization" {
			capture = capture.WithClip(&page.Viewport{
				X: clip.X, Y: clip.Y, Width: clip.Width, Height: clip.Height, Scale: 1,
			})
		}
		var err error
		image, err = capture.Do(ctx)
		if err != nil {
			return err
		}
		return storeScreenshot(ctx, image)
	})
}

func validateArtifactPath(root, outputPath string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("artifact output root is not configured; set --artifact-output-root or GRAFANA_ARTIFACT_OUTPUT_ROOT")
	}
	if !filepath.IsAbs(outputPath) {
		return "", fmt.Errorf("outputPath must be absolute")
	}
	if strings.ToLower(filepath.Ext(outputPath)) != ".png" {
		return "", fmt.Errorf("outputPath must have a .png extension")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve artifact root: %w", err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolve artifact root: %w", err)
	}
	if info, err := os.Stat(rootReal); err != nil || !info.IsDir() {
		if err != nil {
			return "", fmt.Errorf("artifact root is not accessible: %w", err)
		}
		return "", fmt.Errorf("artifact root is not a directory")
	}
	outputAbs := filepath.Clean(outputPath)
	rel, err := filepath.Rel(rootAbs, outputAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("outputPath must be below the configured artifact output root")
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(outputAbs))
	if err != nil {
		return "", fmt.Errorf("output directory must already exist: %w", err)
	}
	realRel, err := filepath.Rel(rootReal, parentReal)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("outputPath resolves outside the configured artifact output root")
	}
	return outputAbs, nil
}

func writeArtifactAtomically(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".mcp-grafana-render-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

var RenderExploreImage = mcpgrafana.MustTool(
	"render_explore_image",
	"Render a Grafana Explore query as a PNG using authenticated local headless Chrome. Writes metadata and the artifact path instead of returning image bytes. Requires --artifact-output-root or GRAFANA_ARTIFACT_OUTPUT_ROOT.",
	renderExploreImage,
	mcp.WithTitleAnnotation("Render Explore image (local browser)"),
	mcp.WithIdempotentHintAnnotation(true),
	mcp.WithReadOnlyHintAnnotation(true),
	mcp.WithDestructiveHintAnnotation(false),
	mcp.WithOpenWorldHintAnnotation(false),
)
