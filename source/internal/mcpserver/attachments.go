package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Only the advertised internal receiver schema is projected; handler validation
// retains the full wire type and no shared schema-cache entry is changed.
func legacyAttachmentTool(tool *mcp.Tool) *mcp.Tool {
	if tool.Name != "wait_for_message" {
		return tool
	}
	schema, ok := tool.OutputSchema.(*jsonschema.Schema)
	if !ok {
		return tool
	}
	copy := *tool
	out := schema.CloneSchemas()
	var strip func(*jsonschema.Schema)
	strip = func(s *jsonschema.Schema) {
		if s == nil {
			return
		}
		delete(s.Properties, "attachment")
		s.PropertyOrder = slices.DeleteFunc(slices.Clone(s.PropertyOrder), func(k string) bool { return k == "attachment" })
		for _, v := range s.Properties {
			strip(v)
		}
		strip(s.Items)
	}
	strip(out)
	copy.OutputSchema = out
	return &copy
}

type attachmentBackend interface {
	UploadAttachment(context.Context, string, string, int64, io.Reader) (protocol.Attachment, error)
	DownloadAttachment(context.Context, string, string) (*attachments.Download, error)
}

var attachmentID = regexp.MustCompile(`^[A-Z2-7]{26}$`)

func attachmentHTTP(servers ProjectServers) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(status int, code string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
		}
		project, ok := projectFromQuery(r.URL.RawQuery)
		if !ok {
			fail(400, "invalid")
			return
		}
		factory, ok := servers.(interface {
			RemoteBackend(context.Context, string) (Backend, error)
		})
		if !ok {
			fail(403, "attachments-disabled")
			return
		}
		backend, err := factory.RemoteBackend(r.Context(), project)
		if err != nil {
			fail(503, "unavailable")
			return
		}
		store, ok := backend.(attachmentBackend)
		if !ok {
			fail(403, "attachments-disabled")
			return
		}
		emitError := func(err error) {
			var e *attachments.Error
			switch {
			case errors.As(err, &e):
				fail(e.Status, e.Code)
			case errors.Is(err, broker.ErrSessionSuspended):
				fail(423, "suspended")
			default:
				fail(403, "forbidden")
			}
		}
		query := r.URL.Query()
		controller := http.NewResponseController(w)
		if r.Method == http.MethodPost && r.URL.Path == "/attachments" {
			if len(query["from"]) != 1 || query.Get("from") == "" || len(r.Header.Values("X-XBroker-Attachment-Name")) != 1 {
				fail(400, "invalid")
				return
			}
			_ = controller.SetReadDeadline(time.Now().Add(10 * time.Minute))
			ref, err := store.UploadAttachment(r.Context(), query.Get("from"), r.Header.Get("X-XBroker-Attachment-Name"), r.ContentLength, http.MaxBytesReader(w, r.Body, attachments.MaxFileBytes))
			if err != nil {
				emitError(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(struct {
				ID     string `json:"id"`
				Size   int64  `json:"size"`
				SHA256 string `json:"sha256"`
			}{ref.ID, ref.Size, ref.SHA256})
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/attachments/") {
			id := strings.TrimPrefix(r.URL.Path, "/attachments/")
			if !attachmentID.MatchString(id) || len(query["recipient"]) != 1 || query.Get("recipient") == "" {
				fail(400, "invalid")
				return
			}
			d, err := store.DownloadAttachment(r.Context(), query.Get("recipient"), id)
			if err != nil {
				emitError(err)
				return
			}
			defer d.Close()
			stop := context.AfterFunc(r.Context(), serverdiag.BackgroundCallback(func() { _ = d.Close() }))
			defer stop()
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Minute))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.FormatInt(d.Ref.Size, 10))
			w.Header().Set("X-XBroker-Attachment-SHA256", d.Ref.SHA256)
			_, _ = io.CopyBuffer(w, d, make([]byte, 32<<10))
			return
		}
		fail(405, "invalid")
	})
}
