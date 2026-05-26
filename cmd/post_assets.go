package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Session-based attachment upload — replicates the browser's drag-and-drop
// flow so images land at github.com/user-attachments/assets/<uuid> and render
// inline in private-repo issue bodies. GitHub's Markdown renderer proxies
// those URLs via camo; it does NOT proxy raw.githubusercontent.com URLs from
// private repos, which is why Contents-API uploads alone don't render.
//
// The endpoints used here are undocumented and reject PATs/OAuth — they need
// a real `user_session` browser cookie. Set GH_SESSION_TOKEN to enable.

const sessionTokenEnv = "GH_SESSION_TOKEN"

type sessionContext struct {
	repoID    int64
	csrfToken string
}

// sessionContextCache memoises (repoID, CSRF) per repo. Today's CLI only ever
// targets one repo per invocation, but caching by key avoids the silent
// cross-repo contamination that a single sync.Once would cause if the caller
// ever passed a second repo. Each key resolves at most once.
type sessionContextCache struct {
	mu      sync.Mutex
	entries map[string]*sessionContextEntry
}

type sessionContextEntry struct {
	once sync.Once
	val  *sessionContext
	err  error
}

var sessionCache sessionContextCache

func sessionToken() string {
	return strings.TrimSpace(os.Getenv(sessionTokenEnv))
}

func getSessionContext(repo string) (*sessionContext, error) {
	sessionCache.mu.Lock()
	if sessionCache.entries == nil {
		sessionCache.entries = make(map[string]*sessionContextEntry)
	}
	entry, ok := sessionCache.entries[repo]
	if !ok {
		entry = &sessionContextEntry{}
		sessionCache.entries[repo] = entry
	}
	sessionCache.mu.Unlock()

	entry.once.Do(func() {
		entry.val, entry.err = loadSessionContext(repo)
	})
	return entry.val, entry.err
}

func loadSessionContext(repo string) (*sessionContext, error) {
	token := sessionToken()
	if token == "" {
		return nil, fmt.Errorf("%s not set", sessionTokenEnv)
	}

	idOut, err := exec.Command("gh", "api", "/repos/"+repo, "--jq", ".id").Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("repo id lookup failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("repo id lookup failed: %w", err)
	}
	repoID, err := strconv.ParseInt(strings.TrimSpace(string(idOut)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("repo id parse failed: %w", err)
	}

	req, err := http.NewRequest("GET", "https://github.com/"+repo, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", buildSessionCookie(token))
	req.Header.Set("User-Agent", "Mozilla/5.0 screwjira")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("repo page fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("repo page HTTP %d — %s likely expired or invalid", resp.StatusCode, sessionTokenEnv)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	m := regexp.MustCompile(`"uploadToken":"([^"]+)"`).FindSubmatch(body)
	if len(m) < 2 {
		return nil, fmt.Errorf("uploadToken not found on repo page — %s likely expired", sessionTokenEnv)
	}

	return &sessionContext{repoID: repoID, csrfToken: string(m[1])}, nil
}

func buildSessionCookie(token string) string {
	return fmt.Sprintf("user_session=%s; __Host-user_session_same_site=%s", token, token)
}

// uploadAttachmentViaSession runs the 3-step browser upload flow and returns
// a https://github.com/user-attachments/assets/<uuid> URL.
func uploadAttachmentViaSession(repo, filename, mimeType string, data []byte) (string, error) {
	ctx, err := getSessionContext(repo)
	if err != nil {
		return "", err
	}
	token := sessionToken()

	policy, err := requestUploadPolicy(repo, ctx, token, filename, mimeType, len(data))
	if err != nil {
		return "", fmt.Errorf("policy: %w", err)
	}

	if err := uploadToS3(policy, filename, data); err != nil {
		return "", fmt.Errorf("s3: %w", err)
	}

	href, err := finalizeUpload(repo, token, policy.Asset.ID, policy.AssetUploadAuthenticityToken)
	if err != nil {
		return "", fmt.Errorf("finalize: %w", err)
	}
	return href, nil
}

type uploadPolicy struct {
	UploadURL string `json:"upload_url"`
	Asset     struct {
		ID   int64  `json:"id"`
		Href string `json:"href"`
	} `json:"asset"`
	Form                         orderedFields `json:"form"`
	AssetUploadAuthenticityToken string        `json:"asset_upload_authenticity_token"`
}

// orderedFields preserves the key order from the policy response's `form`
// object. S3 multipart uploads require the `file` field to be last, and
// while AWS doesn't strictly require the others to be in a specific order,
// preserving GitHub's order keeps behaviour aligned with the browser flow.
type orderedFields [][2]string

func (o *orderedFields) UnmarshalJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected object, got %v", tok)
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		var val string
		if err := dec.Decode(&val); err != nil {
			return err
		}
		*o = append(*o, [2]string{fmt.Sprintf("%v", keyTok), val})
	}
	return nil
}

func requestUploadPolicy(repo string, ctx *sessionContext, token, filename, mimeType string, size int) (*uploadPolicy, error) {
	form := url.Values{}
	form.Set("name", filename)
	form.Set("size", strconv.Itoa(size))
	form.Set("content_type", mimeType)
	form.Set("repository_id", strconv.FormatInt(ctx.repoID, 10))
	form.Set("authenticity_token", ctx.csrfToken)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vs := range form {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				return nil, err
			}
		}
	}
	mw.Close()

	req, _ := http.NewRequest("POST", "https://github.com/upload/policies/assets", &buf)
	setSessionHeaders(req, repo, token, mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var p uploadPolicy
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if p.UploadURL == "" || p.Asset.ID == 0 {
		return nil, fmt.Errorf("incomplete policy response")
	}
	return &p, nil
}

func uploadToS3(policy *uploadPolicy, filename string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, kv := range policy.Form {
		if err := mw.WriteField(kv[0], kv[1]); err != nil {
			return err
		}
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	mw.Close()

	req, _ := http.NewRequest("POST", policy.UploadURL, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "https://github.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return nil
}

func finalizeUpload(repo, token string, assetID int64, csrfToken string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("authenticity_token", csrfToken); err != nil {
		return "", err
	}
	mw.Close()

	url := fmt.Sprintf("https://github.com/upload/assets/%d", assetID)
	req, _ := http.NewRequest("PUT", url, &buf)
	setSessionHeaders(req, repo, token, mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var out struct {
		Href string `json:"href"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Href == "" {
		return "", fmt.Errorf("empty href in response")
	}
	return out.Href, nil
}

func setSessionHeaders(req *http.Request, repo, token, contentType string) {
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://github.com")
	req.Header.Set("Referer", "https://github.com/"+repo)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Cookie", buildSessionCookie(token))
	req.Header.Set("User-Agent", "Mozilla/5.0 screwjira")
}
