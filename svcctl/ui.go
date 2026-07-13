//go:build go1.22

package svcctl

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"rules_itest/runner"
)

const pageTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8" />
    <title>Service Logs</title>
    <style>
        body {
            display: flex;
            height: 100vh;
            margin: 0;
            font-family: sans-serif;
        }

        #sidebar {
            width: 200px;
            background-color: #f0f0f0;
            border-right: 1px solid #ccc;
            padding: 10px;
            box-sizing: border-box;
        }

        #main {
            flex: 1;
            display: flex;
            flex-direction: column;
        }

        #log {
            flex: 1;
            padding: 10px;
            overflow-y: scroll;
            white-space: pre-wrap;
            background-color: #000;
            color: #0f0;
            font-family: monospace;
            font-size: 14px;
            border-top: 1px solid #ccc;
        }

        .service {
            cursor: pointer;
            padding: 5px;
            border-radius: 3px;
            margin-bottom: 5px;
        }

        .service:hover {
            background-color: #ddd;
        }

        .service.active {
            background-color: #bbb;
            font-weight: bold;
        }
    </style>
</head>
<body>
    <div id="sidebar">
        {{ range $index, $svc := .Services }}
            <div class="service {{ if eq $index 0 }}active{{ end }}" data-service="{{ $svc }}">{{ $svc }}</div>
        {{ end }}
    </div>

    <div id="main">
        <div id="log">Connecting...</div>
    </div>

    <script>
        const logDiv = document.getElementById('log');
        let offset = 0;
        let controller = null;
        let currentService = document.querySelector('.service.active').getAttribute('data-service');

        async function fetchLogs() {
            controller = new AbortController();
            const signal = controller.signal;

            try {
       	        const url = new URL('/v0/log', window.location);
                url.searchParams.set('service', currentService);
                url.searchParams.set('offset', offset);
                const response = await fetch(url, { signal });
                const decoder = new TextDecoder("utf-8");

                for await (const chunk of response.body) {
                    const text = decoder.decode(chunk, { stream: true });
                    const isAtBottom = logDiv.scrollTop + logDiv.clientHeight >= logDiv.scrollHeight - 5;

                    logDiv.textContent += text;
                    offset += chunk.length;

                    if (isAtBottom) {
                        logDiv.scrollTop = logDiv.scrollHeight;
                    }
                }
            } catch (err) {
                if (err.name === 'AbortError') {
                    console.log('Fetch aborted');
                } else {
                    console.error('Fetch logs failed:', err);
                    logDiv.textContent += '\n--- connection error ---\n';
                }
            }
        }

        function start() {
            if (controller) {
                controller.abort();
            }
            logDiv.textContent = '';
            offset = 0;
            fetchLogs();
        }

        // Sidebar click
        document.querySelectorAll('.service').forEach(el => {
            el.addEventListener('click', () => {
                document.querySelectorAll('.service').forEach(s => s.classList.remove('active'));
                el.classList.add('active');

                currentService = el.getAttribute('data-service');
                start();
            });
        });

        start();
    </script>
</body>
</html>
`

var tmpl = template.Must(template.New("page").Parse(pageTemplate))

func handleUI(ctx context.Context, r *runner.Runner, _ chan error, w http.ResponseWriter, req *http.Request) {
	err := tmpl.Execute(w, struct{ Services []string }{Services: r.ServiceLabels()})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handleLog(ctx context.Context, r *runner.Runner, _ chan error, w http.ResponseWriter, req *http.Request) {
	instance := r.GetInstance(req.URL.Query().Get("service"))

	offsetStr := req.URL.Query().Get("offset")
	var offset int64
	if offsetStr != "" {
		offset, _ = strconv.ParseInt(offsetStr, 10, 64)
	}

	f, err := os.Open(instance.LogPath())
	if err != nil {
		http.Error(w, "failed to open log file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	_, err = f.Seek(offset, io.SeekStart)
	if err != nil {
		http.Error(w, "failed to seek log file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Transfer-Encoding", "chunked")

	buf := make([]byte, 4096)
	fmt.Println("ZZZZ starting transfer")
	for {
		n, err := f.Read(buf)
		fmt.Println("ZZZ finished read", instance.LogPath())
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				// client disconnected
				fmt.Printf("Client disconnected: %v\n", writeErr)
				return
			}
			fmt.Println("writing bytes", n)
			if flusher, ok := w.(http.Flusher); ok {
				fmt.Println("flushing", n)
				flusher.Flush()
			}
		}

		if err == io.EOF {
			time.Sleep(100 * time.Millisecond)
			continue
		} else if err != nil {
			// Some other error
			fmt.Printf("Error reading log file: %v\n", err)
			return
		}
	}
}
