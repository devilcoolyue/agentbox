package syncclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentbox/internal/syncproto"
)

var ErrTransport = errors.New("sync network or TLS connection failed")
var ErrProtocol = errors.New("invalid sync server response")

const maxManifestJSON int64 = 64 << 20

// HTTPError deliberately excludes response bodies, URLs and credentials.
// A failed request is never converted into an empty tree or automatically retried.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("sync request rejected (HTTP %d)", e.Status) }
func (e *HTTPError) Is(target error) bool {
	return e.Status == http.StatusConflict && target == syncproto.ErrChanged || e.Status == http.StatusRequestEntityTooLarge && target == syncproto.ErrLimit
}

// Remote is a native-only transport primitive. It neither authorizes a sync plan
// nor starts background work; the executor must enforce capability, binding,
// preview confirmation and lease fencing before performing any local mutation.
// In particular, constructing it must not expose automatic sync while sync=0.
type Remote struct {
	serverID string
	base     *url.URL
	token    string
	client   *http.Client
}

func NewRemote(address, token string, allowHTTP bool) (*Remote, error) {
	normalized, err := syncproto.NormalizeServer(address)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return nil, syncproto.ErrInvalid
	}
	host := base.Hostname()
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if base.Scheme == "http" && !local && !allowHTTP {
		return nil, syncproto.ErrInvalid
	}
	if token == "" || len(token) > 4096 || strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r >= 127 }) >= 0 {
		return nil, syncproto.ErrInvalid
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 65 * time.Second,
		IdleConnTimeout: 90 * time.Second, MaxIdleConns: 4, MaxConnsPerHost: 4,
		DisableCompression: true,
	}
	return &Remote{base: base, token: token, client: &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (r *Remote) Close() { r.client.CloseIdleConnections() }

func syncID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}
func (r *Remote) request(ctx context.Context, method, workspace, endpoint string, query url.Values, body []byte, lease string) (*http.Response, error) {
	if !syncID(workspace) {
		return nil, syncproto.ErrInvalid
	}
	address := *r.base
	address.Path += "api/sessions/" + workspace + "/sync/" + endpoint
	address.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, address.String(), bytes.NewReader(body))
	if err != nil {
		return nil, syncproto.ErrInvalid
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	if r.serverID != "" {
		request.Header.Set("X-Agentbox-Server-ID", r.serverID)
	}
	request.Header.Set("Accept-Encoding", "identity")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if lease != "" {
		request.Header.Set("X-Agentbox-Sync-Lease", lease)
	}
	response, err := r.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, transportFailure(err)
	}
	if r.serverID != "" && response.Header.Get("X-Agentbox-Server-ID") != r.serverID {
		response.Body.Close()
		return nil, ErrBinding
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, &HTTPError{response.StatusCode}
	}
	return response, nil
}
func decodeRemote(response *http.Response, limit int64, value any) error {
	defer response.Body.Close()
	kind, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" || response.ContentLength > limit {
		return ErrProtocol
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return transportFailure(err)
	}
	if int64(len(data)) > limit || json.Unmarshal(data, value) != nil {
		return ErrProtocol
	}
	return nil
}
func (r *Remote) Manifest(ctx context.Context, workspace, project string) (syncproto.ManifestResponse, error) {
	var manifest syncproto.ManifestResponse
	if !syncID(project) {
		return manifest, syncproto.ErrInvalid
	}
	response, err := r.request(ctx, http.MethodGet, workspace, "manifest", url.Values{"project": {project}}, nil, "")
	if err != nil {
		return manifest, err
	}
	if err = decodeRemote(response, maxManifestJSON, &manifest); err != nil {
		return syncproto.ManifestResponse{}, err
	}
	if manifest.Validate(project) != nil {
		return syncproto.ManifestResponse{}, ErrProtocol
	}
	return manifest, nil
}
func (r *Remote) Acquire(ctx context.Context, workspace, project, device string) (syncproto.Lease, error) {
	return r.lease(ctx, "acquire", syncproto.Lease{Workspace: workspace, Project: project, Device: device})
}
func (r *Remote) Renew(ctx context.Context, lease syncproto.Lease) (syncproto.Lease, error) {
	return r.lease(ctx, "renew", lease)
}
func (r *Remote) Release(ctx context.Context, lease syncproto.Lease) error {
	_, err := r.lease(ctx, "release", lease)
	return err
}
func (r *Remote) lease(ctx context.Context, action string, lease syncproto.Lease) (syncproto.Lease, error) {
	if !syncID(lease.Project) || !syncID(lease.Device) || action != "acquire" && (!syncID(lease.Token) || !syncID(lease.Generation)) {
		return syncproto.Lease{}, syncproto.ErrInvalid
	}
	body, _ := json.Marshal(struct {
		Project    string `json:"project"`
		Device     string `json:"device"`
		Action     string `json:"action"`
		Generation string `json:"generation"`
	}{lease.Project, lease.Device, action, lease.Generation})
	response, err := r.request(ctx, http.MethodPost, lease.Workspace, "lease", nil, body, lease.Token)
	if err != nil {
		return syncproto.Lease{}, err
	}
	if action == "release" {
		var result struct {
			OK bool `json:"ok"`
		}
		if err = decodeRemote(response, 16<<10, &result); err != nil {
			return syncproto.Lease{}, err
		}
		if !result.OK {
			return syncproto.Lease{}, ErrProtocol
		}
		return syncproto.Lease{}, nil
	}
	var result syncproto.Lease
	if err = decodeRemote(response, 16<<10, &result); err != nil {
		return syncproto.Lease{}, err
	}
	if result.Workspace != lease.Workspace || result.Project != lease.Project || result.Device != lease.Device || !syncID(result.Token) || !syncID(result.Generation) || result.Expires.IsZero() || result.Path != "." && !syncproto.ValidPath(result.Path) {
		return syncproto.Lease{}, ErrProtocol
	}
	if action == "renew" && (result.Token != lease.Token || result.Generation != lease.Generation || result.Path != lease.Path) {
		return syncproto.Lease{}, ErrProtocol
	}
	return result, nil
}

// File returns a stream that checks its size and SHA-256 before returning EOF.
// Callers must consume it fully into private staging and close it on every path;
// bytes read before EOF are unverified and must not be published to user files.
func (r *Remote) File(ctx context.Context, workspace string, request syncproto.FileRequest) (io.ReadCloser, error) {
	if request.Validate() != nil || !syncID(request.Project) {
		return nil, syncproto.ErrInvalid
	}
	q := url.Values{"project": {request.Project}, "project_revision": {strconv.FormatInt(request.Revision, 10)}, "rules_hash": {request.RulesHash}, "path": {request.Path}, "hash": {request.Expected.Hash}, "size": {strconv.FormatInt(request.Expected.Size, 10)}, "executable": {strconv.FormatBool(request.Expected.Executable)}}
	response, err := r.request(ctx, http.MethodGet, workspace, "file", q, nil, "")
	if err != nil {
		return nil, err
	}
	return verifiedResponse(ctx, response, request.Expected)
}

func verifiedResponse(ctx context.Context, response *http.Response, expected syncproto.Entry) (io.ReadCloser, error) {
	kind, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || kind != "application/octet-stream" || response.Header.Get("Content-Encoding") != "" || response.ContentLength != expected.Size || response.Header.Get("ETag") != `"`+expected.Hash+`"` {
		response.Body.Close()
		return nil, ErrProtocol
	}
	return &verifiedDownload{body: response.Body, expected: expected, hash: sha256.New(), ctx: ctx}, nil
}

type verifiedDownload struct {
	body     io.ReadCloser
	expected syncproto.Entry
	hash     hash.Hash
	read     int64
	terminal error
	ctx      context.Context
}

func (r *verifiedDownload) Close() error { return r.body.Close() }
func (r *verifiedDownload) Read(p []byte) (int, error) {
	if r.terminal != nil {
		return 0, r.terminal
	}
	if err := r.ctx.Err(); err != nil {
		r.terminal = err
		return 0, err
	}
	// Read at most one byte beyond the promised length, including chunked peers.
	if int64(len(p)) > r.expected.Size-r.read+1 {
		p = p[:r.expected.Size-r.read+1]
	}
	n, err := r.body.Read(p)
	r.read += int64(n)
	_, _ = r.hash.Write(p[:n])
	if r.read > r.expected.Size {
		err = ErrProtocol
	}
	if errors.Is(err, io.EOF) && (r.read != r.expected.Size || hex.EncodeToString(r.hash.Sum(nil)) != r.expected.Hash) {
		err = ErrProtocol
	}
	if err != nil {
		if err != io.EOF && err != ErrProtocol {
			err = ErrTransport
			if r.ctx.Err() != nil {
				err = r.ctx.Err()
			}
		}
		r.terminal = err
	}
	return n, err
}

// UncertainError carries the durable operation ID and recovery availability.
// The executor must rescan/reconcile rather than resubmit with a new ID blindly.
type UncertainError struct{ Result syncproto.MutationResult }

func (e *UncertainError) Error() string { return "sync operation requires reconciliation" }

func (r *Remote) Apply(ctx context.Context, workspace string, lease syncproto.Lease, operation syncproto.Mutation, source io.Reader) (syncproto.MutationResult, error) {
	var result syncproto.MutationResult
	if !syncID(workspace) || operation.Validate() != nil || lease.Workspace != workspace || lease.Project != operation.Project || lease.Device != operation.Device || lease.Generation != operation.Generation || !syncID(lease.Token) {
		return result, syncproto.ErrInvalid
	}
	raw, err := json.Marshal(operation)
	if err != nil {
		return result, syncproto.ErrInvalid
	}
	metadata := base64.RawURLEncoding.EncodeToString(raw)
	if len(metadata) > 16<<10 {
		return result, syncproto.ErrInvalid
	}
	size := int64(0)
	if operation.After != nil {
		size = operation.After.Size
	}
	if source == nil {
		source = bytes.NewReader(nil)
	}
	address := *r.base
	address.Path += "api/sessions/" + workspace + "/sync/apply"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address.String(), io.LimitReader(source, size+1))
	if err != nil {
		return result, syncproto.ErrInvalid
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	if r.serverID != "" {
		request.Header.Set("X-Agentbox-Server-ID", r.serverID)
	}
	request.Header.Set("X-Agentbox-Sync-Lease", lease.Token)
	request.Header.Set("X-Agentbox-Sync-Request", metadata)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := r.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, ErrTransport // Publication might have happened; retain the operation ID.
	}
	if r.serverID != "" && response.Header.Get("X-Agentbox-Server-ID") != r.serverID {
		response.Body.Close()
		return result, ErrBinding
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusConflict {
			var conflict struct {
				Operation syncproto.MutationResult `json:"operation"`
			}
			if decodeRemote(response, 16<<10, &conflict) == nil && conflict.Operation.ID == operation.ID && conflict.Operation.Status == "uncertain" {
				return conflict.Operation, &UncertainError{conflict.Operation}
			}
		} else {
			response.Body.Close()
		}
		return result, &HTTPError{response.StatusCode}
	}
	if err = decodeRemote(response, 16<<10, &result); err != nil {
		return syncproto.MutationResult{}, err
	}
	if result.ID != operation.ID || result.Status != "applied" {
		return syncproto.MutationResult{}, ErrProtocol
	}
	return result, nil
}

func (r *Remote) Operation(ctx context.Context, workspace, id string) (syncproto.OperationStatus, error) {
	var result syncproto.OperationStatus
	if !syncproto.ValidOperationID(id) {
		return result, syncproto.ErrInvalid
	}
	response, err := r.request(ctx, http.MethodGet, workspace, "operations/"+id, nil, nil, "")
	if err != nil {
		return result, err
	}
	if err = decodeRemote(response, 16<<10, &result); err != nil {
		return syncproto.OperationStatus{}, err
	}
	if result.Validate(id) != nil {
		return syncproto.OperationStatus{}, ErrProtocol
	}
	return result, nil
}
func (r *Remote) Recovery(ctx context.Context, workspace string, status syncproto.OperationStatus) (io.ReadCloser, error) {
	if status.Validate(status.Operation.ID) != nil || status.Before == nil || status.Before.Kind != "file" || status.Retirement != "" || !status.Operation.Recovery {
		return nil, syncproto.ErrInvalid
	}
	response, err := r.request(ctx, http.MethodGet, workspace, "operations/"+status.Operation.ID+"/before", nil, nil, "")
	if err != nil {
		return nil, err
	}
	return verifiedResponse(ctx, response, *status.Before)
}
