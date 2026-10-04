// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package eks

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"istio.io/istio/pkg/test/util/assert"
)

const testServer = "https://ABCDEF0123456789.gr7.us-west-2.eks.amazonaws.com"

type fakeSource struct {
	calls  int
	region string
	err    error
}

func (f *fakeSource) Token(context.Context) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return fmt.Sprintf("token-%d", f.calls), nil
}

func withFakeSource(t *testing.T) *fakeSource {
	src := &fakeSource{}
	orig := newTokenSource
	newTokenSource = func(_, region, _ string) (tokenSource, error) {
		src.region = region
		return src, nil
	}
	t.Cleanup(func() { newTokenSource = orig })
	return src
}

func TestNewAuthProvider(t *testing.T) {
	cases := []struct {
		name       string
		server     string
		config     map[string]string
		wantRegion string
		wantErr    string
	}{
		{
			name:       "region derived from server",
			server:     testServer,
			config:     map[string]string{ConfigClusterName: "c1"},
			wantRegion: "us-west-2",
		},
		{
			name:       "explicit region",
			server:     testServer,
			config:     map[string]string{ConfigClusterName: "c1", ConfigRegion: "eu-west-1", ConfigRoleARN: "arn:aws:iam::1:role/r"},
			wantRegion: "eu-west-1",
		},
		{
			name:       "china partition",
			server:     "https://ABCDEF.yl4.cn-north-1.eks.amazonaws.com.cn:443",
			config:     map[string]string{ConfigClusterName: "c1"},
			wantRegion: "cn-north-1",
		},
		{
			name:    "missing cluster name",
			server:  testServer,
			config:  map[string]string{},
			wantErr: "cluster-name",
		},
		{
			name:    "unknown config key",
			server:  testServer,
			config:  map[string]string{ConfigClusterName: "c1", "profile": "prod"},
			wantErr: "unsupported config key",
		},
		{
			name:    "non eks server",
			server:  "https://attacker.example.com",
			config:  map[string]string{ConfigClusterName: "c1"},
			wantErr: "not an EKS endpoint",
		},
		{
			name:    "suffix lookalike",
			server:  "https://foo.eks.amazonaws.com.example.com",
			config:  map[string]string{ConfigClusterName: "c1"},
			wantErr: "not an EKS endpoint",
		},
		{
			name:    "plain http",
			server:  "http://ABCDEF.gr7.us-west-2.eks.amazonaws.com",
			config:  map[string]string{ConfigClusterName: "c1"},
			wantErr: "https",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			src := withFakeSource(t)
			_, err := newAuthProvider(tt.server, tt.config, nil)
			if tt.wantErr != "" {
				assert.Error(t, err)
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, src.region, tt.wantRegion)
		})
	}
}

type recordingRoundTripper struct {
	auth []string
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func TestTokenCaching(t *testing.T) {
	src := withFakeSource(t)
	ap, err := newAuthProvider(testServer, map[string]string{ConfigClusterName: "c1"}, nil)
	assert.NoError(t, err)
	p := ap.(*authProvider)
	now := time.Unix(0, 0)
	p.now = func() time.Time { return now }

	rec := &recordingRoundTripper{}
	rt := p.WrapTransport(rec)
	do := func() {
		req, _ := http.NewRequest(http.MethodGet, testServer+"/api", nil)
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("original request must not be mutated")
		}
	}

	do()
	now = now.Add(tokenRefreshInterval - time.Second)
	do()
	now = now.Add(2 * time.Second)
	do()
	assert.Equal(t, rec.auth, []string{"Bearer token-1", "Bearer token-1", "Bearer token-2"})
	assert.Equal(t, src.calls, 2)
}

func TestTokenError(t *testing.T) {
	src := withFakeSource(t)
	src.err = fmt.Errorf("no credentials")
	ap, err := newAuthProvider(testServer, map[string]string{ConfigClusterName: "c1"}, nil)
	assert.NoError(t, err)
	req, _ := http.NewRequest(http.MethodGet, testServer+"/api", nil)
	_, err = ap.WrapTransport(&recordingRoundTripper{}).RoundTrip(req)
	assert.Error(t, err)
}

func TestPresignToken(t *testing.T) {
	cfg := aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
	}
	tok, err := presignToken(context.Background(), newPresigner(cfg, ""), "my-cluster")
	assert.NoError(t, err)
	if !strings.HasPrefix(tok, tokenPrefix) {
		t.Fatalf("token missing prefix: %s", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, tokenPrefix))
	assert.NoError(t, err)
	u, err := url.Parse(string(raw))
	assert.NoError(t, err)
	assert.Equal(t, u.Host, "sts.us-west-2.amazonaws.com")
	q := u.Query()
	assert.Equal(t, q.Get("Action"), "GetCallerIdentity")
	if !strings.Contains(q.Get("X-Amz-SignedHeaders"), clusterIDHeader) {
		t.Fatalf("cluster id header must be signed, got %q", q.Get("X-Amz-SignedHeaders"))
	}
	if !strings.HasPrefix(q.Get("X-Amz-Credential"), "AKIDEXAMPLE/") {
		t.Fatalf("unexpected credential %q", q.Get("X-Amz-Credential"))
	}
}

func TestRegisteredWithClientGo(t *testing.T) {
	withFakeSource(t)
	cfg := api.NewConfig()
	cfg.Clusters["c"] = &api.Cluster{Server: testServer}
	cfg.AuthInfos["u"] = &api.AuthInfo{AuthProvider: &api.AuthProviderConfig{
		Name:   AuthProviderName,
		Config: map[string]string{ConfigClusterName: "c1"},
	}}
	cfg.Contexts["ctx"] = &api.Context{Cluster: "c", AuthInfo: "u"}
	cfg.CurrentContext = "ctx"
	restConfig, err := clientcmd.NewDefaultClientConfig(*cfg, &clientcmd.ConfigOverrides{}).ClientConfig()
	assert.NoError(t, err)
	// Building the transport instantiates the auth provider via the registered factory.
	_, err = rest.TransportFor(restConfig)
	assert.NoError(t, err)
}
