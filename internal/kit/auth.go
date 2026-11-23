package kit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// HeaderAuth sends a credential in a fixed header. prefix is the text before
// the credential, for example "Bearer " or "SSWS ".
func HeaderAuth(name, prefix string, s Secret) Authenticator {
	return func(_ context.Context, req *http.Request) error {
		req.Header.Set(name, prefix+s.Reveal())
		return nil
	}
}

// BasicAuth sends HTTP basic credentials.
func BasicAuth(user string, pass Secret) Authenticator {
	return func(_ context.Context, req *http.Request) error {
		req.SetBasicAuth(user, pass.Reveal())
		return nil
	}
}

// ClientCredentials gets an OAuth 2.0 access token with the client credentials
// grant, and keeps it until shortly before it expires. The token endpoint is
// fixed by the server configuration and never comes from a tool input.
type ClientCredentials struct {
	TokenURL string
	ID       string
	Secret   Secret
	// Extra holds more form fields, for example "scope" or "resource".
	Extra url.Values
	// ClientSecretInBody sends the client id and secret as form fields. When
	// false, the token request uses HTTP basic authentication.
	ClientSecretInBody bool
	// OmitGrantType leaves out the grant_type field, for a vendor whose token
	// endpoint takes only the client id and the client secret.
	OmitGrantType bool
	HTTPClient    *http.Client
	// Now replaces the clock. Tests set it.
	Now func() time.Time

	mu      sync.Mutex
	token   Secret
	expires time.Time
}

// refreshMargin renews a token this long before it expires.
const refreshMargin = 60 * time.Second

// Token returns a valid access token.
func (c *ClientCredentials) Token(ctx context.Context) (Secret, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if c.token != "" && now().Add(refreshMargin).Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{}
	if !c.OmitGrantType {
		form.Set("grant_type", "client_credentials")
	}
	for k, v := range c.Extra {
		form[k] = v
	}
	if c.ClientSecretInBody {
		form.Set("client_id", c.ID)
		form.Set("client_secret", c.Secret.Reveal())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("build token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if !c.ClientSecretInBody {
		req.SetBasicAuth(url.QueryEscape(c.ID), url.QueryEscape(c.Secret.Reveal()))
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	hcc := *hc
	hcc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hcc.Do(req)
	if err != nil {
		// The cause gives the class: a transport failure is unavailable, or a
		// timeout. A tool call that wrote before this failure is outcome_unknown.
		return "", &tokenError{msg: "token endpoint is not reachable", cause: &transportError{err: err}}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// The body is not shown. A token endpoint can echo a credential.
		return "", &tokenError{msg: fmt.Sprintf("token endpoint answered HTTP %d", resp.StatusCode),
			cause: &APIError{Status: resp.StatusCode, RateLimited: resp.StatusCode == http.StatusTooManyRequests}}
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&tr); err != nil || tr.AccessToken == "" {
		return "", errors.New("token endpoint answer has no access token")
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 300
	}
	c.token = Secret(tr.AccessToken)
	c.expires = now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, nil
}

// tokenError is a failure of the token endpoint. Its text is fixed, because a
// token endpoint can echo a credential. The cause gives the class of the error.
type tokenError struct {
	msg   string
	cause error
}

func (e *tokenError) Error() string { return e.msg }
func (e *tokenError) Unwrap() error { return e.cause }

// Authenticator returns an Authenticator that sends the token as a bearer
// token.
func (c *ClientCredentials) Authenticator() Authenticator {
	return func(ctx context.Context, req *http.Request) error {
		t, err := c.Token(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+t.Reveal())
		return nil
	}
}

// tenantFormat is a Microsoft Entra tenant: a GUID or a domain name.
var tenantFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,254}$`)

// MicrosoftClientCredentials returns the token source of an Entra ID app for
// the client credentials grant (OAuth 2.0 v2 endpoint). authority is the login
// host, for example https://login.microsoftonline.com. scope is the resource
// with "/.default", for example https://graph.microsoft.com/.default.
func MicrosoftClientCredentials(authority *url.URL, tenant, clientID string, secret Secret, scope string) (*ClientCredentials, error) {
	if !tenantFormat.MatchString(tenant) {
		return nil, errors.New("the tenant id has the wrong format")
	}
	return &ClientCredentials{
		TokenURL:           authority.String() + Path(tenant, "oauth2", "v2.0", "token"),
		ID:                 clientID,
		Secret:             secret,
		Extra:              url.Values{"scope": {scope}},
		ClientSecretInBody: true,
	}, nil
}
