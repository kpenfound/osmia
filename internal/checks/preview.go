package checks

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/redact"
)

// ChromiumImage pins the browser and its Node API together. This image is amd64.
const ChromiumImage = "ghcr.io/puppeteer/puppeteer:24.31.0@sha256:9853364130f73ad2df7afe3c9ae86c2d7c637ca2efd67a7260bacbc8141c214b"

// PreviewCapture describes one static capture of a project's Dagger Service.
type PreviewCapture struct {
	Call     []string
	Port     int
	Path     string
	Viewport string
	FullPage bool
}

// Preview runs only Dagger clients on the host. The returned Service is bound
// inside the browser container; neither invocation publishes a host port.
func (d DaggerChecks) Preview(ctx context.Context, dir string, in PreviewCapture) ([]byte, error) {
	output := &checkOutput{limit: 16 << 10}
	fail := func(err error) ([]byte, error) {
		secrets := redact.KnownSecrets()
		tail := output.String()
		// The bounded buffer can begin halfway through a secret. Remove any
		// matching suffix before redacting complete values and bounding the result.
		if output.truncated {
			for _, secret := range secrets {
				for i := 1; i < len(secret); i++ {
					if strings.HasPrefix(tail, secret[i:]) {
						tail = "[redacted]" + tail[len(secret)-i:]
						break
					}
				}
			}
		}
		prefix := "preview capture: " + redact.Secrets(err.Error(), secrets...)
		if len(prefix) > 1024 {
			prefix = prefix[:1024]
		}
		tail = redact.Secrets(tail, secrets...)
		budget := (16 << 10) - len(prefix) - 1
		if len(tail) > budget {
			tail = tail[len(tail)-budget:]
			for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
				tail = tail[1:]
			}
		}
		return nil, fmt.Errorf("%s", strings.TrimSpace(prefix+"\n"+tail))
	}
	id := &checkOutput{limit: 64 << 10}
	args := append(append([]string{"call"}, in.Call...), "id")
	exit, _, err := d.daggerOutput(ctx, dir, Identity{}, false, id, output, args...)
	if err != nil {
		return fail(err)
	}
	if exit != 0 {
		output.Write([]byte(id.String()))
		return fail(fmt.Errorf("dagger Service call exited %d", exit))
	}
	service := strings.TrimSpace(id.String())
	if service == "" || id.truncated || strings.ContainsAny(service, "\r\n\x00") {
		return fail(fmt.Errorf("dagger did not return one Service ID"))
	}
	width, height := 1440, 900
	if in.Viewport == "phone" {
		width, height = 390, 844
	}
	options, _ := json.Marshal(map[string]any{"url": fmt.Sprintf("http://preview:%d%s", in.Port, in.Path), "width": width, "height": height, "phone": in.Viewport == "phone", "fullPage": in.FullPage})
	script := `const puppeteer = require('puppeteer'); const options = ` + string(options) + `;
(async () => {
 const browser = await puppeteer.launch({args: ['--no-sandbox']});
 try {
  const page = await browser.newPage();
  await page.setViewport({width: options.width, height: options.height, isMobile: options.phone, hasTouch: options.phone});
  await page.goto(options.url, {waitUntil: 'networkidle0'});
  await page.screenshot({path: '/tmp/osmia-preview.png', fullPage: options.fullPage});
 } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });`
	// Dagger's string-slice flag uses CSV, including quotes and embedded commas.
	var execArgs strings.Builder
	writer := csv.NewWriter(&execArgs)
	_ = writer.Write([]string{"node", "-e", script})
	writer.Flush()
	out, err := os.MkdirTemp("", "osmia-preview-output-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(out)
	file := filepath.Join(out, "preview.png")
	args = []string{"core", "container", "--platform=linux/amd64", "from", "--address", ChromiumImage,
		"with-workdir", "--path=/home/pptruser",
		"with-service-binding", "--alias=preview", "--service", service,
		"with-exec", "--args", strings.TrimSuffix(execArgs.String(), "\n"),
		"file", "--path=/tmp/osmia-preview.png", "export", "--path", file}
	exit, _, err = d.dagger(ctx, dir, Identity{}, false, output, args...)
	if err != nil {
		return fail(err)
	}
	if exit != 0 {
		return fail(fmt.Errorf("dagger Chromium capture exited %d", exit))
	}
	f, err := os.Open(file)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (5<<20)+1))
	if err != nil {
		return fail(err)
	}
	if len(data) > 5<<20 {
		return fail(fmt.Errorf("preview image exceeds 5 MiB"))
	}
	return data, nil
}
