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

// Package eks provides a client-go auth provider plugin that authenticates to Amazon EKS
// clusters using AWS IAM credentials, without executing any external command.
//
// The plugin is configured through a kubeconfig auth-provider stanza:
//
//	users:
//	- name: remote
//	  user:
//	    auth-provider:
//	      name: eks
//	      config:
//	        cluster-name: my-cluster            # required
//	        region: us-west-2                   # optional, derived from the server address if unset
//	        role-arn: arn:aws:iam::1:role/r     # optional, role to assume before signing
//
// Credentials are resolved with the AWS SDK default credential chain of the running process
// (for istiod this is typically IRSA or EKS Pod Identity).
package eks

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"

	"istio.io/istio/pkg/log"
)

const (
	// AuthProviderName is the name of the kubeconfig auth-provider implemented by this package.
	AuthProviderName = "eks"

	ConfigClusterName = "cluster-name"
	ConfigRegion      = "region"
	ConfigRoleARN     = "role-arn"

	tokenPrefix     = "k8s-aws-v1."
	clusterIDHeader = "x-k8s-aws-id"

	// EKS accepts tokens for 15 minutes after signing. Refresh well before that.
	tokenRefreshInterval = 10 * time.Minute
	tokenTimeout         = 30 * time.Second
)

// allowedHostSuffixes restricts which API servers receive the signed token. This limits where
// istiod's IAM identity can be presented if a remote secret points at an unexpected server.
var allowedHostSuffixes = []string{".eks.amazonaws.com", ".eks.amazonaws.com.cn"}

func init() {
	if err := rest.RegisterAuthProviderPlugin(AuthProviderName, newAuthProvider); err != nil {
		log.Errorf("failed to register %s auth provider: %v", AuthProviderName, err)
	}
}

// tokenSource generates a bearer token for an EKS cluster.
type tokenSource interface {
	Token(ctx context.Context) (string, error)
}

// newTokenSource is overridden in tests.
var newTokenSource = func(clusterName, region, roleARN string) (tokenSource, error) {
	return &stsTokenSource{clusterName: clusterName, region: region, roleARN: roleARN}, nil
}

type authProvider struct {
	source tokenSource
	now    func() time.Time

	mu        sync.Mutex
	token     string
	fetchedAt time.Time
}

var _ rest.AuthProvider = &authProvider{}

func newAuthProvider(clusterAddress string, cfg map[string]string, _ rest.AuthProviderConfigPersister) (rest.AuthProvider, error) {
	clusterName := cfg[ConfigClusterName]
	if clusterName == "" {
		return nil, fmt.Errorf("%s auth provider: %q must be set", AuthProviderName, ConfigClusterName)
	}
	for k := range cfg {
		switch k {
		case ConfigClusterName, ConfigRegion, ConfigRoleARN:
		default:
			return nil, fmt.Errorf("%s auth provider: unsupported config key %q", AuthProviderName, k)
		}
	}
	host, err := validateServer(clusterAddress)
	if err != nil {
		return nil, err
	}
	region := cfg[ConfigRegion]
	if region == "" {
		region = regionFromHost(host)
	}
	if region == "" {
		return nil, fmt.Errorf("%s auth provider: %q must be set, it could not be derived from server %q",
			AuthProviderName, ConfigRegion, clusterAddress)
	}
	src, err := newTokenSource(clusterName, region, cfg[ConfigRoleARN])
	if err != nil {
		return nil, err
	}
	return &authProvider{source: src, now: time.Now}, nil
}

// validateServer ensures the token is only ever sent to an EKS API server endpoint.
func validateServer(clusterAddress string) (string, error) {
	u, err := url.Parse(clusterAddress)
	if err != nil {
		return "", fmt.Errorf("%s auth provider: invalid server %q: %v", AuthProviderName, clusterAddress, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("%s auth provider: server %q must use https", AuthProviderName, clusterAddress)
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range allowedHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return host, nil
		}
	}
	return "", fmt.Errorf("%s auth provider: server %q is not an EKS endpoint (expected suffix %v)",
		AuthProviderName, clusterAddress, allowedHostSuffixes)
}

// regionFromHost extracts the region from an EKS endpoint such as
// ABCDEF.gr7.us-west-2.eks.amazonaws.com.
func regionFromHost(host string) string {
	labels := strings.Split(host, ".")
	for i := 1; i < len(labels); i++ {
		if labels[i] == "eks" {
			return labels[i-1]
		}
	}
	return ""
}

func (p *authProvider) WrapTransport(rt http.RoundTripper) http.RoundTripper {
	return &bearerRoundTripper{provider: p, rt: rt}
}

func (p *authProvider) Login() error {
	return fmt.Errorf("%s auth provider does not support interactive login", AuthProviderName)
}

func (p *authProvider) getToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && p.now().Sub(p.fetchedAt) < tokenRefreshInterval {
		return p.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	tok, err := p.source.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to generate EKS token: %v", err)
	}
	p.token = tok
	p.fetchedAt = p.now()
	return tok, nil
}

type bearerRoundTripper struct {
	provider *authProvider
	rt       http.RoundTripper
}

var _ utilnet.RoundTripperWrapper = &bearerRoundTripper{}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return b.rt.RoundTrip(req)
	}
	tok, err := b.provider.getToken(req.Context())
	if err != nil {
		return nil, err
	}
	req = utilnet.CloneRequest(req)
	req.Header.Set("Authorization", "Bearer "+tok)
	return b.rt.RoundTrip(req)
}

func (b *bearerRoundTripper) WrappedRoundTripper() http.RoundTripper { return b.rt }

// stsTokenSource signs an STS GetCallerIdentity request, the format accepted by EKS
// (and aws-iam-authenticator). Signing happens locally; no request is sent to STS unless
// a role must be assumed or credentials must be fetched.
// Calls are serialized by authProvider.
type stsTokenSource struct {
	clusterName string
	region      string
	roleARN     string

	presigner *sts.PresignClient
}

func newPresigner(cfg aws.Config, roleARN string) *sts.PresignClient {
	if roleARN != "" {
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), roleARN,
			func(o *stscreds.AssumeRoleOptions) {
				o.RoleSessionName = "istiod-eks-auth"
			}))
	}
	return sts.NewPresignClient(sts.NewFromConfig(cfg))
}

func (s *stsTokenSource) Token(ctx context.Context) (string, error) {
	if s.presigner == nil {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(s.region))
		if err != nil {
			return "", fmt.Errorf("failed to load AWS config: %v", err)
		}
		s.presigner = newPresigner(cfg, s.roleARN)
	}
	return presignToken(ctx, s.presigner, s.clusterName)
}

func presignToken(ctx context.Context, presigner *sts.PresignClient, clusterName string) (string, error) {
	req, err := presigner.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, func(opts *sts.Options) {
			opts.APIOptions = append(opts.APIOptions, smithyhttp.SetHeaderValue(clusterIDHeader, clusterName))
		})
	})
	if err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(req.URL)), nil
}
