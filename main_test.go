// Copyright 2018 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"cmp"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/prometheus/discovery"
	"github.com/stretchr/testify/require"
)

const (
	actionDescribeCacheClusters = "DescribeCacheClusters"
	actionListTagsForResource   = "ListTagsForResource"
	actionAssumeRole            = "AssumeRole"
)

type endpointXML struct {
	Address *string `xml:"Address,omitempty"`
	Port    *int32  `xml:"Port,omitempty"`
}

type cacheNodeXML struct {
	CacheNodeID              *string      `xml:"CacheNodeId,omitempty"`
	CacheNodeStatus          *string      `xml:"CacheNodeStatus,omitempty"`
	CustomerAvailabilityZone *string      `xml:"CustomerAvailabilityZone,omitempty"`
	Endpoint                 *endpointXML `xml:"Endpoint,omitempty"`
}

type cacheParameterGroupXML struct {
	Name *string `xml:"CacheParameterGroupName,omitempty"`
}

type cacheClusterXML struct {
	ARN                       *string                 `xml:"ARN,omitempty"`
	CacheClusterID            *string                 `xml:"CacheClusterId,omitempty"`
	CacheClusterStatus        *string                 `xml:"CacheClusterStatus,omitempty"`
	CacheNodeType             *string                 `xml:"CacheNodeType,omitempty"`
	CacheParameterGroup       *cacheParameterGroupXML `xml:"CacheParameterGroup,omitempty"`
	CacheSubnetGroupName      *string                 `xml:"CacheSubnetGroupName,omitempty"`
	Engine                    *string                 `xml:"Engine,omitempty"`
	EngineVersion             *string                 `xml:"EngineVersion,omitempty"`
	PreferredAvailabilityZone *string                 `xml:"PreferredAvailabilityZone,omitempty"`
	ReplicationGroupID        *string                 `xml:"ReplicationGroupId,omitempty"`
	CacheNodes                []cacheNodeXML          `xml:"CacheNodes>CacheNode,omitempty"`
}

type tagXML struct {
	Key   *string `xml:"Key,omitempty"`
	Value *string `xml:"Value,omitempty"`
}

type elasticacheDataStore struct {
	region        string
	cacheClusters []cacheClusterXML
	tags          map[string][]tagXML
}

type errorResponseXML struct {
	XMLName   xml.Name `xml:"ErrorResponse"`
	Type      string   `xml:"Error>Type"`
	Code      string   `xml:"Error>Code"`
	Message   string   `xml:"Error>Message"`
	RequestID string   `xml:"RequestId"`
}

type assumeRoleCredentialsXML struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

type assumeRoleResultXML struct {
	Credentials assumeRoleCredentialsXML `xml:"Credentials"`
}

type assumeRoleResponseXML struct {
	XMLName xml.Name            `xml:"AssumeRoleResponse"`
	Result  assumeRoleResultXML `xml:"AssumeRoleResult"`
}

type describeCacheClustersResultXML struct {
	Clusters []cacheClusterXML `xml:"CacheClusters>CacheCluster"`
	Marker   string            `xml:"Marker,omitempty"`
}

type describeCacheClustersResponseXML struct {
	XMLName xml.Name                       `xml:"DescribeCacheClustersResponse"`
	Result  describeCacheClustersResultXML `xml:"DescribeCacheClustersResult"`
}

type listTagsForResourceResultXML struct {
	TagList []tagXML `xml:"TagList>Tag"`
}

type listTagsForResourceResponseXML struct {
	XMLName xml.Name                     `xml:"ListTagsForResourceResponse"`
	Result  listTagsForResourceResultXML `xml:"ListTagsForResourceResult"`
}

func mustMarshalXML(v any) string {
	b, err := xml.Marshal(v)
	if err != nil {
		panic(err)
	}

	return string(b)
}

func writeMockAPIResponse(w http.ResponseWriter, statusCode int, body string) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(statusCode)
	_, _ = io.WriteString(w, body)
}

func writeMockAPIError(w http.ResponseWriter, statusCode int, code, message string) {
	writeMockAPIResponse(w, statusCode, mustMarshalXML(errorResponseXML{
		Type:      "Sender",
		Code:      code,
		Message:   message,
		RequestID: "mock",
	}))
}

func assumeRoleXML() string {
	return mustMarshalXML(assumeRoleResponseXML{
		Result: assumeRoleResultXML{
			Credentials: assumeRoleCredentialsXML{
				AccessKeyID:     "ASIATEST",
				SecretAccessKey: "secret",
				SessionToken:    "token",
				Expiration:      "2030-01-01T00:00:00Z",
			},
		},
	})
}

func describeCacheClustersXML(marker string, clusters ...cacheClusterXML) string {
	return mustMarshalXML(describeCacheClustersResponseXML{
		Result: describeCacheClustersResultXML{
			Clusters: clusters,
			Marker:   marker,
		},
	})
}

func listTagsForResourceXML(tags ...tagXML) string {
	return mustMarshalXML(listTagsForResourceResponseXML{
		Result: listTagsForResourceResultXML{TagList: tags},
	})
}

type mockElasticacheRequest struct {
	Action string
	Params url.Values
}

type mockElasticacheFailure struct {
	StatusCode int
	Code       string
}

type mockElasticacheAPI struct {
	server *httptest.Server
	t      testing.TB

	onRequest func()

	record bool

	mu               sync.Mutex
	requests         []mockElasticacheRequest
	handlers         map[string]func(http.ResponseWriter, url.Values)
	failures         map[string]mockElasticacheFailure
	resourceFailures map[string]map[string]mockElasticacheFailure
}

func newMockElasticacheAPI(t testing.TB) *mockElasticacheAPI {
	t.Helper()

	_, benchmarking := t.(*testing.B)

	m := &mockElasticacheAPI{
		t:                t,
		record:           !benchmarking,
		handlers:         map[string]func(http.ResponseWriter, url.Values){},
		failures:         map[string]mockElasticacheFailure{},
		resourceFailures: map[string]map[string]mockElasticacheFailure{},
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	t.Cleanup(m.server.Close)

	return m
}

func (m *mockElasticacheAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		m.t.Errorf("mock API: parsing request form: %v", err)
		writeMockAPIError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	action := r.PostForm.Get("Action")

	// The resource a request targets is named ResourceName or CacheClusterId,
	// depending on the action.
	resource := cmp.Or(r.PostForm.Get("ResourceName"), r.PostForm.Get("CacheClusterId"))

	m.mu.Lock()
	if m.record {
		m.requests = append(m.requests, mockElasticacheRequest{Action: action, Params: r.PostForm})
	}
	handler := m.handlers[action]
	failure, failed := m.failures[action]
	if resourceFailure, resourceFailed := m.resourceFailures[action][resource]; resourceFailed {
		failure, failed = resourceFailure, true
	}
	onRequest := m.onRequest
	m.mu.Unlock()

	if onRequest != nil {
		onRequest()
	}

	if failed {
		writeMockAPIError(w, failure.StatusCode, failure.Code, "injected failure for "+action)
		return
	}

	if handler == nil {
		writeMockAPIError(w, http.StatusBadRequest, "InvalidAction", "unexpected ElastiCache action "+action)
		return
	}

	handler(w, r.PostForm)
}

func (m *mockElasticacheAPI) handle(action string, handler func(http.ResponseWriter, url.Values)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.handlers[action] = handler
}

func (m *mockElasticacheAPI) fail(action string, statusCode int, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.failures[action] = mockElasticacheFailure{StatusCode: statusCode, Code: code}
}

func (m *mockElasticacheAPI) failResource(action, resource string, statusCode int, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.resourceFailures[action] == nil {
		m.resourceFailures[action] = map[string]mockElasticacheFailure{}
	}
	m.resourceFailures[action][resource] = mockElasticacheFailure{StatusCode: statusCode, Code: code}
}

func (m *mockElasticacheAPI) recordedRequests() []mockElasticacheRequest {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.requests)
}

func (m *mockElasticacheAPI) requestsFor(action string) []mockElasticacheRequest {
	var requests []mockElasticacheRequest
	for _, request := range m.recordedRequests() {
		if request.Action == action {
			requests = append(requests, request)
		}
	}

	return requests
}

func (m *mockElasticacheAPI) client(region string) *elasticache.Client {
	m.t.Helper()

	return elasticache.NewFromConfig(aws.Config{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint: aws.String(m.server.URL),
	})
}

func (m *mockElasticacheAPI) handleDescribeCacheClusters(clusters ...cacheClusterXML) {
	body := describeCacheClustersXML("", clusters...)

	m.handle(actionDescribeCacheClusters, func(w http.ResponseWriter, params url.Values) {
		id := params.Get("CacheClusterId")
		if id == "" {
			writeMockAPIResponse(w, http.StatusOK, body)
			return
		}

		// The CacheClusterId filter is not case sensitive.
		filtered := slices.DeleteFunc(slices.Clone(clusters), func(c cacheClusterXML) bool {
			return !strings.EqualFold(aws.ToString(c.CacheClusterID), id)
		})
		if len(filtered) == 0 {
			writeMockAPIError(w, http.StatusNotFound, "CacheClusterNotFound", "Cache cluster not found: "+id)
			return
		}

		writeMockAPIResponse(w, http.StatusOK, describeCacheClustersXML("", filtered...))
	})
}

func (m *mockElasticacheAPI) handleListTagsForResource(tagsByARN map[string][]tagXML, clusters ...cacheClusterXML) {
	bodies := make(map[string]string, len(tagsByARN))
	for arn, tags := range tagsByARN {
		bodies[arn] = listTagsForResourceXML(tags...)
	}

	existing := make(map[string]bool, len(clusters))
	for _, cluster := range clusters {
		if arn := aws.ToString(cluster.ARN); arn != "" {
			existing[arn] = true
		}
	}

	emptyBody := listTagsForResourceXML()

	m.handle(actionListTagsForResource, func(w http.ResponseWriter, params url.Values) {
		resourceName := params.Get("ResourceName")
		if resourceName == "" {
			writeMockAPIError(w, http.StatusBadRequest, "InvalidParameterValue", "ResourceName must not be empty")
			return
		}

		body, ok := bodies[resourceName]
		if !ok {
			if !existing[resourceName] {
				writeMockAPIError(w, http.StatusNotFound, "CacheClusterNotFound", "Cache cluster not found: "+resourceName)
				return
			}

			// A cluster without tags returns an empty TagList.
			body = emptyBody
		}

		writeMockAPIResponse(w, http.StatusOK, body)
	})
}

func fullyPopulatedCacheCluster() cacheClusterXML {
	return cacheClusterXML{
		ARN:                       new("arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001"),
		CacheClusterID:            new("my-cluster-001"),
		CacheClusterStatus:        new("available"),
		CacheNodeType:             new("cache.t3.micro"),
		CacheParameterGroup:       &cacheParameterGroupXML{Name: new("default.redis7")},
		CacheSubnetGroupName:      new("my-subnet-group"),
		Engine:                    new("redis"),
		EngineVersion:             new("7.1"),
		PreferredAvailabilityZone: new("us-east-1a"),
		CacheNodes: []cacheNodeXML{{
			CacheNodeID:              new("0001"),
			CacheNodeStatus:          new("available"),
			CustomerAvailabilityZone: new("us-east-1a"),
			Endpoint: &endpointXML{
				Address: new("my-cluster-001.abc123.0001.use1.cache.amazonaws.com"),
				Port:    aws.Int32(6379),
			},
		}},
	}
}

func secondCacheNode() cacheNodeXML {
	return cacheNodeXML{
		CacheNodeID:              new("0002"),
		CacheNodeStatus:          new("available"),
		CustomerAvailabilityZone: new("us-east-1b"),
		Endpoint: &endpointXML{
			Address: new("my-cluster-001.abc123.0002.use1.cache.amazonaws.com"),
			Port:    aws.Int32(6379),
		},
	}
}

func secondCacheCluster() cacheClusterXML {
	cluster := fullyPopulatedCacheCluster()
	cluster.ARN = new("arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-002")
	cluster.CacheClusterID = new("my-cluster-002")

	return cluster
}

func elasticacheFixture(clusterCount, nodeCount int) *elasticacheDataStore {
	data := &elasticacheDataStore{
		region: "us-east-1",
		tags:   make(map[string][]tagXML, clusterCount),
	}

	for c := range clusterCount {
		id := "cluster-" + strconv.Itoa(c)
		arn := "arn:aws:elasticache:us-east-1:123456789012:cluster:" + id
		cluster := fullyPopulatedCacheCluster()
		cluster.ARN = new(arn)
		cluster.CacheClusterID = new(id)
		cluster.CacheNodes = make([]cacheNodeXML, 0, nodeCount)
		for n := range nodeCount {
			cluster.CacheNodes = append(cluster.CacheNodes, cacheNodeXML{
				CacheNodeID: new(fmt.Sprintf("%04d", n+1)),
				Endpoint: &endpointXML{
					Address: new(fmt.Sprintf("%s-%04d.use1.cache.amazonaws.com", id, n+1)),
					Port:    aws.Int32(6379),
				},
			})
		}
		data.cacheClusters = append(data.cacheClusters, cluster)
		data.tags[arn] = []tagXML{{Key: new("Environment"), Value: new("bench")}}
	}

	return data
}

func elasticacheTestDiscoveryWithAPI(t testing.TB, data *elasticacheDataStore) (*ElasticacheDiscovery, *mockElasticacheAPI) {
	t.Helper()

	api := newMockElasticacheAPI(t)
	api.handleDescribeCacheClusters(data.cacheClusters...)
	api.handleListTagsForResource(data.tags, data.cacheClusters...)

	return &ElasticacheDiscovery{
		logger:      promslog.NewNopLogger(),
		cfg:         &ElasticacheSDConfig{Region: data.region},
		elasticache: api.client(data.region),
	}, api
}

func elasticacheTestDiscovery(t testing.TB, data *elasticacheDataStore) *ElasticacheDiscovery {
	t.Helper()

	disc, _ := elasticacheTestDiscoveryWithAPI(t, data)

	return disc
}

func clearAWSEnvironment(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"AWS_CONFIG_FILE",
		"AWS_SHARED_CREDENTIALS_FILE",
		"AWS_PROFILE",
		"AWS_REGION",
		"AWS_DEFAULT_REGION",
		"AWS_ENDPOINT_URL",
		"AWS_ENDPOINT_URL_ELASTICACHE",
		"AWS_ENDPOINT_URL_STS",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func TestElasticacheSDConfigName(t *testing.T) {
	t.Parallel()

	require.Equal(t, "elasticache", (&ElasticacheSDConfig{}).Name())
}

func TestNewElasticacheDiscovery(t *testing.T) {
	t.Parallel()

	conf := &ElasticacheSDConfig{Region: "us-east-1", RefreshInterval: time.Minute}

	for _, tt := range []struct {
		name    string
		metrics discovery.DiscovererMetrics
		wantErr string
	}{
		{
			name:    "ValidMetrics",
			metrics: conf.NewDiscovererMetrics(nil, discovery.NewRefreshMetrics(prometheus.NewRegistry())),
		},
		{
			name:    "InvalidMetricsType",
			metrics: &discovery.NoopDiscovererMetrics{},
			wantErr: "invalid discovery metrics type",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			disc, err := NewElasticacheDiscovery(conf, promslog.NewNopLogger(), tt.metrics)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, disc)
		})
	}
}

func TestNewElasticacheDiscoveryNilLogger(t *testing.T) {
	t.Parallel()

	api := newMockElasticacheAPI(t)
	api.handleDescribeCacheClusters(fullyPopulatedCacheCluster())

	conf := &ElasticacheSDConfig{Region: "us-east-1", RefreshInterval: time.Minute}
	disc, err := NewElasticacheDiscovery(
		conf,
		nil,
		conf.NewDiscovererMetrics(nil, discovery.NewRefreshMetrics(prometheus.NewRegistry())),
	)
	require.NoError(t, err)
	require.NotNil(t, disc.logger)

	disc.elasticache = api.client("us-east-1")
	_, err = disc.refresh(t.Context())
	require.NoError(t, err)
}

func TestElasticacheDiscoveryClientCached(t *testing.T) {
	t.Parallel()

	client := &elasticache.Client{}
	disc := &ElasticacheDiscovery{
		cfg:         &ElasticacheSDConfig{Region: "us-east-1"},
		elasticache: client,
	}

	got, err := disc.elasticacheClient(t.Context())
	require.NoError(t, err)
	require.Same(t, client, got)
}

func TestElasticacheDiscoveryClientFromEnvironment(t *testing.T) {
	clearAWSEnvironment(t)

	api := newMockElasticacheAPI(t)
	api.handleDescribeCacheClusters(fullyPopulatedCacheCluster())
	t.Setenv("AWS_ENDPOINT_URL", api.server.URL)

	disc := &ElasticacheDiscovery{
		logger: promslog.NewNopLogger(),
		cfg: &ElasticacheSDConfig{
			Region:          "us-east-1",
			AccessKey:       "test",
			SecretKey:       "test",
			RefreshInterval: time.Minute,
		},
	}

	tgs, err := disc.refresh(t.Context())
	require.NoError(t, err)
	require.NotNil(t, disc.elasticache, "refresh must build the client from the environment")
	require.Len(t, tgs, 1)
	require.Equal(t, "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001/0001", tgs[0].Source)
}

func TestElasticacheDiscoveryClientAssumesRole(t *testing.T) {
	clearAWSEnvironment(t)

	const roleARN = "arn:aws:iam::123456789012:role/test"

	api := newMockElasticacheAPI(t)
	api.handleDescribeCacheClusters(fullyPopulatedCacheCluster())
	api.handle(actionAssumeRole, func(w http.ResponseWriter, _ url.Values) {
		writeMockAPIResponse(w, http.StatusOK, assumeRoleXML())
	})
	t.Setenv("AWS_ENDPOINT_URL", api.server.URL)
	t.Setenv("AWS_ENDPOINT_URL_STS", api.server.URL)

	disc := &ElasticacheDiscovery{
		logger: promslog.NewNopLogger(),
		cfg: &ElasticacheSDConfig{
			Region:          "us-east-1",
			AccessKey:       "test",
			SecretKey:       "test",
			RoleARN:         roleARN,
			RefreshInterval: time.Minute,
		},
	}

	tgs, err := disc.refresh(t.Context())
	require.NoError(t, err)
	require.Len(t, tgs, 1)

	assumeRoles := api.requestsFor(actionAssumeRole)
	require.NotEmpty(t, assumeRoles, "refresh must assume the configured role")
	require.Equal(t, roleARN, assumeRoles[len(assumeRoles)-1].Params.Get("RoleArn"))
}

func TestElasticacheRefreshClientResetOnError(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name          string
		statusCode    int
		code          string
		wantClientNil bool
	}{
		{name: "AuthFailure", statusCode: http.StatusForbidden, code: "AuthFailure", wantClientNil: true},
		{name: "UnauthorizedOperation", statusCode: http.StatusForbidden, code: "UnauthorizedOperation", wantClientNil: true},
		{name: "OtherError", statusCode: http.StatusBadRequest, code: "InvalidParameterValue", wantClientNil: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			disc, api := elasticacheTestDiscoveryWithAPI(t, &elasticacheDataStore{
				region:        "us-east-1",
				cacheClusters: []cacheClusterXML{fullyPopulatedCacheCluster()},
			})
			api.fail(actionDescribeCacheClusters, tt.statusCode, tt.code)

			_, err := disc.refresh(t.Context())
			require.Error(t, err)
			if tt.wantClientNil {
				require.Nil(t, disc.elasticache, "an auth failure must drop the cached client")
			} else {
				require.NotNil(t, disc.elasticache, "a non-auth error must keep the cached client")
			}
		})
	}
}

func TestElasticacheDiscoveryClientMissingProfile(t *testing.T) {
	clearAWSEnvironment(t)

	configFile := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(configFile, []byte("[default]\nregion = us-east-1\n"), 0o600))
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_REGION", "us-east-1")

	disc := &ElasticacheDiscovery{
		logger: promslog.NewNopLogger(),
		cfg: &ElasticacheSDConfig{
			Region:          "us-east-1",
			Profile:         "does-not-exist",
			RefreshInterval: time.Minute,
		},
	}

	_, err := disc.refresh(t.Context())
	require.ErrorContains(t, err, "could not load aws default config")
	require.Nil(t, disc.elasticache)
}

func TestElasticacheRefreshTargetsAndLabels(t *testing.T) {
	t.Parallel()

	const (
		clusterARN = "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001"
		clusterID  = "my-cluster-001"
	)

	cluster := fullyPopulatedCacheCluster()
	cluster.CacheNodes = append(cluster.CacheNodes, secondCacheNode())

	tgs, err := elasticacheTestDiscovery(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{cluster},
		tags: map[string][]tagXML{
			clusterARN: {
				{Key: new("Environment"), Value: new("production")},
				{Key: new("my.tag/key"), Value: new("value")},
			},
		},
	}).refresh(t.Context())
	require.NoError(t, err)
	require.Len(t, tgs, 2)

	require.Equal(t, clusterARN+"/0001", tgs[0].Source)
	node1 := model.LabelSet{
		"__meta_elasticache_cache_cluster_id":            clusterID,
		"__meta_elasticache_cache_cluster_status":        "available",
		"__meta_elasticache_cache_node_type":             "cache.t3.micro",
		"__meta_elasticache_cache_parameter_group_name":  "default.redis7",
		"__meta_elasticache_cache_subnet_group_name":     "my-subnet-group",
		"__meta_elasticache_engine":                      "redis",
		"__meta_elasticache_engine_version":              "7.1",
		"__meta_elasticache_preferred_availability_zone": "us-east-1a",
		"__meta_elasticache_tag_Environment":             "production",
		"__meta_elasticache_tag_my_tag_key":              "value",
		"__meta_elasticache_cache_node_id":               "0001",
		"__meta_elasticache_cache_node_status":           "available",
		"__meta_elasticache_customer_availability_zone":  "us-east-1a",
		"__meta_elasticache_endpoint_address":            "my-cluster-001.abc123.0001.use1.cache.amazonaws.com",
		"__meta_elasticache_endpoint_port":               "6379",
		"__address__":                                    "undefined",
	}
	require.Equal(t, node1, tgs[0].Labels)
	require.Equal(t, []model.LabelSet{{model.AddressLabel: "undefined"}}, tgs[0].Targets)

	require.Equal(t, clusterARN+"/0002", tgs[1].Source)
	node2 := node1.Clone()
	node2["__meta_elasticache_cache_node_id"] = "0002"
	node2["__meta_elasticache_customer_availability_zone"] = "us-east-1b"
	node2["__meta_elasticache_endpoint_address"] = "my-cluster-001.abc123.0002.use1.cache.amazonaws.com"
	require.Equal(t, node2, tgs[1].Labels)
	require.Equal(t, []model.LabelSet{{model.AddressLabel: "undefined"}}, tgs[1].Targets)
}

func TestElasticacheRefreshReplicationGroupID(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name               string
		replicationGroupID *string
		expected           model.LabelValue
	}{
		{name: "Present", replicationGroupID: new("my-replication-group"), expected: "my-replication-group"},
		{name: "Absent", replicationGroupID: nil, expected: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cluster := fullyPopulatedCacheCluster()
			cluster.ReplicationGroupID = tt.replicationGroupID

			tgs, err := elasticacheTestDiscovery(t, &elasticacheDataStore{
				region:        "us-east-1",
				cacheClusters: []cacheClusterXML{cluster},
			}).refresh(t.Context())
			require.NoError(t, err)
			require.Len(t, tgs, 1)
			if tt.expected == "" {
				require.NotContains(t, tgs[0].Labels, model.LabelName(ecLabelReplicationGroupID))
			} else {
				require.Equal(t, tt.expected, tgs[0].Labels[ecLabelReplicationGroupID])
			}
		})
	}
}

func TestElasticacheRefreshPaginates(t *testing.T) {
	t.Parallel()

	first := fullyPopulatedCacheCluster()
	second := secondCacheCluster()

	disc, api := elasticacheTestDiscoveryWithAPI(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{first, second},
	})
	api.handle(actionDescribeCacheClusters, func(w http.ResponseWriter, params url.Values) {
		if params.Get("Marker") == "" {
			writeMockAPIResponse(w, http.StatusOK, describeCacheClustersXML("next-page", first))
			return
		}
		writeMockAPIResponse(w, http.StatusOK, describeCacheClustersXML("", second))
	})

	tgs, err := disc.refresh(t.Context())
	require.NoError(t, err)
	require.Len(t, tgs, 2)
	require.Equal(t, "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001/0001", tgs[0].Source)
	require.Equal(t, "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-002/0001", tgs[1].Source)

	describes := api.requestsFor(actionDescribeCacheClusters)
	require.Len(t, describes, 2)
	require.Empty(t, describes[0].Params.Get("Marker"))
	require.Equal(t, "true", describes[0].Params.Get("ShowCacheNodeInfo"))
	require.Empty(t, describes[0].Params.Get("CacheClusterId"))
	require.Equal(t, "false", describes[0].Params.Get("ShowCacheClustersNotInReplicationGroups"))
	require.Equal(t, "next-page", describes[1].Params.Get("Marker"))
}

func TestElasticacheRefreshCacheClusterID(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		id   string
	}{
		{name: "ExactCase", id: "my-cluster-001"},
		{name: "DifferentCase", id: "MY-CLUSTER-001"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			other := secondCacheCluster()

			disc, _ := elasticacheTestDiscoveryWithAPI(t, &elasticacheDataStore{
				region:        "us-east-1",
				cacheClusters: []cacheClusterXML{fullyPopulatedCacheCluster(), other},
			})
			disc.cfg.cacheClusterID = tt.id

			tgs, err := disc.refresh(t.Context())
			require.NoError(t, err)
			require.Len(t, tgs, 1)
			require.Equal(t, "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001/0001", tgs[0].Source)
		})
	}
}

func TestElasticacheRefreshCacheClusterNotFound(t *testing.T) {
	t.Parallel()

	disc, _ := elasticacheTestDiscoveryWithAPI(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{fullyPopulatedCacheCluster()},
	})
	disc.cfg.cacheClusterID = "no-such-cluster"

	_, err := disc.refresh(t.Context())
	require.ErrorContains(t, err, "could not describe cache clusters")

	var notFound *types.CacheClusterNotFoundFault
	require.ErrorAs(t, err, &notFound)
}

func TestElasticacheRefreshRemovesGoneTargets(t *testing.T) {
	t.Parallel()

	cluster := fullyPopulatedCacheCluster()
	cluster.CacheNodes = append(cluster.CacheNodes, secondCacheNode())

	disc, api := elasticacheTestDiscoveryWithAPI(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{cluster},
	})

	tgs, err := disc.refresh(t.Context())
	require.NoError(t, err)
	require.Len(t, tgs, 2)

	cluster.CacheNodes = cluster.CacheNodes[:1]
	api.handleDescribeCacheClusters(cluster)

	tgs, err = disc.refresh(t.Context())
	require.NoError(t, err)
	require.Len(t, tgs, 2)
	require.Equal(t, "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001/0001", tgs[0].Source)
	require.NotEmpty(t, tgs[0].Targets)
	require.Equal(t, "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001/0002", tgs[1].Source)
	require.Empty(t, tgs[1].Labels)
	require.Empty(t, tgs[1].Targets)
}

func TestElasticacheRefreshReusesLastTags(t *testing.T) {
	t.Parallel()

	const clusterARN = "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001"

	disc, api := elasticacheTestDiscoveryWithAPI(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{fullyPopulatedCacheCluster()},
		tags: map[string][]tagXML{
			clusterARN: {{Key: new("Environment"), Value: new("production")}},
		},
	})

	tgs, err := disc.refresh(t.Context())
	require.NoError(t, err)
	require.Equal(t, model.LabelValue("production"), tgs[0].Labels[ecLabelTag+"Environment"])

	api.fail(actionListTagsForResource, http.StatusForbidden, "AccessDenied")

	tgs, err = disc.refresh(t.Context())
	require.NoError(t, err)
	require.Equal(t, model.LabelValue("production"), tgs[0].Labels[ecLabelTag+"Environment"])
}

func TestElasticacheRefreshClusterWithoutNodes(t *testing.T) {
	t.Parallel()

	cluster := fullyPopulatedCacheCluster()
	cluster.CacheNodes = nil

	tgs, err := elasticacheTestDiscovery(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{cluster},
	}).refresh(t.Context())
	require.NoError(t, err)
	require.Empty(t, tgs)
}

func TestElasticacheRefreshNilOptionalFields(t *testing.T) {
	t.Parallel()

	const clusterARN = "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001"

	for _, tc := range []struct {
		name   string
		mutate func(*cacheClusterXML)
		absent model.LabelName
	}{
		{
			name:   "NilCacheClusterId",
			mutate: func(c *cacheClusterXML) { c.CacheClusterID = nil },
			absent: ecLabelCacheClusterID,
		},
		{
			name:   "NilCacheClusterStatus",
			mutate: func(c *cacheClusterXML) { c.CacheClusterStatus = nil },
			absent: ecLabelCacheClusterStatus,
		},
		{
			name:   "NilCacheNodeType",
			mutate: func(c *cacheClusterXML) { c.CacheNodeType = nil },
			absent: ecLabelCacheNodeType,
		},
		{
			name:   "NilCacheParameterGroup",
			mutate: func(c *cacheClusterXML) { c.CacheParameterGroup = nil },
			absent: ecLabelCacheParameterGroupName,
		},
		{
			name:   "NilCacheParameterGroupName",
			mutate: func(c *cacheClusterXML) { c.CacheParameterGroup.Name = nil },
			absent: ecLabelCacheParameterGroupName,
		},
		{
			name:   "NilCacheSubnetGroupName",
			mutate: func(c *cacheClusterXML) { c.CacheSubnetGroupName = nil },
			absent: ecLabelCacheSubnetGroupName,
		},
		{
			name:   "NilEngine",
			mutate: func(c *cacheClusterXML) { c.Engine = nil },
			absent: ecLabelEngine,
		},
		{
			name:   "NilEngineVersion",
			mutate: func(c *cacheClusterXML) { c.EngineVersion = nil },
			absent: ecLabelEngineVersion,
		},
		{
			name:   "NilPreferredAvailabilityZone",
			mutate: func(c *cacheClusterXML) { c.PreferredAvailabilityZone = nil },
			absent: ecLabelPreferredAZ,
		},
		{
			name:   "NilCacheNodeId",
			mutate: func(c *cacheClusterXML) { c.CacheNodes[0].CacheNodeID = nil },
			absent: ecLabelCacheNodeID,
		},
		{
			name:   "NilCacheNodeStatus",
			mutate: func(c *cacheClusterXML) { c.CacheNodes[0].CacheNodeStatus = nil },
			absent: ecLabelCacheNodeStatus,
		},
		{
			name:   "NilCustomerAvailabilityZone",
			mutate: func(c *cacheClusterXML) { c.CacheNodes[0].CustomerAvailabilityZone = nil },
			absent: ecLabelCustomerAZ,
		},
		{
			name:   "NilEndpoint",
			mutate: func(c *cacheClusterXML) { c.CacheNodes[0].Endpoint = nil },
			absent: ecLabelEndpointAddress,
		},
		{
			name:   "NilEndpointAddress",
			mutate: func(c *cacheClusterXML) { c.CacheNodes[0].Endpoint.Address = nil },
			absent: ecLabelEndpointAddress,
		},
		{
			name:   "NilEndpointPort",
			mutate: func(c *cacheClusterXML) { c.CacheNodes[0].Endpoint.Port = nil },
			absent: ecLabelEndpointPort,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cluster := fullyPopulatedCacheCluster()
			tc.mutate(&cluster)

			tgs, err := elasticacheTestDiscovery(t, &elasticacheDataStore{
				region:        "us-east-1",
				cacheClusters: []cacheClusterXML{cluster},
				tags: map[string][]tagXML{
					clusterARN: {{Key: new("Environment"), Value: new("production")}},
				},
			}).refresh(t.Context())
			require.NoError(t, err)
			require.Len(t, tgs, 1)
			require.Len(t, tgs[0].Targets, 1, "the node must still be discovered when an optional field is absent")
			require.NotContains(t, tgs[0].Labels, tc.absent, "the label sourced from the absent field must be omitted")
		})
	}
}

func TestElasticacheRefreshNilTagFields(t *testing.T) {
	t.Parallel()

	const clusterARN = "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001"

	tgs, err := elasticacheTestDiscovery(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{fullyPopulatedCacheCluster()},
		tags: map[string][]tagXML{
			clusterARN: {
				{Key: new("Environment"), Value: new("production")},
				{Key: nil, Value: new("orphan-value")},
				{Key: new("OrphanKey"), Value: nil},
			},
		},
	}).refresh(t.Context())
	require.NoError(t, err)
	require.Len(t, tgs, 1)
	require.Equal(t, model.LabelValue("production"), tgs[0].Labels[ecLabelTag+"Environment"])
	require.NotContains(t, tgs[0].Labels, model.LabelName(ecLabelTag+"OrphanKey"))
	require.NotContains(t, tgs[0].Labels, model.LabelName(ecLabelTag), "a tag without a key must be skipped")
}

func TestElasticacheRefreshMissingARN(t *testing.T) {
	t.Parallel()

	cluster := fullyPopulatedCacheCluster()
	cluster.ARN = nil
	cluster.CacheNodes = append(cluster.CacheNodes, secondCacheNode())

	disc, _ := elasticacheTestDiscoveryWithAPI(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{cluster},
	})

	var logs strings.Builder
	disc.logger = promslog.New(&promslog.Config{Writer: &logs})

	tgs, err := disc.refresh(t.Context())
	require.NoError(t, err)
	require.Len(t, tgs, 2)
	require.Equal(t, "my-cluster-001/0001", tgs[0].Source)
	require.Equal(t, "my-cluster-001/0002", tgs[1].Source)

	require.NotContains(t, logs.String(), "could not list tags",
		"a cluster without an ARN must skip the tag lookup")
}

func TestElasticacheRefreshTagFailureOnOneCluster(t *testing.T) {
	t.Parallel()

	const (
		firstARN  = "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-001"
		secondARN = "arn:aws:elasticache:us-east-1:123456789012:cluster:my-cluster-002"
	)

	second := secondCacheCluster()

	disc, api := elasticacheTestDiscoveryWithAPI(t, &elasticacheDataStore{
		region:        "us-east-1",
		cacheClusters: []cacheClusterXML{fullyPopulatedCacheCluster(), second},
		tags: map[string][]tagXML{
			firstARN:  {{Key: new("Environment"), Value: new("production")}},
			secondARN: {{Key: new("Environment"), Value: new("staging")}},
		},
	})
	api.failResource(actionListTagsForResource, firstARN, http.StatusForbidden, "AccessDenied")

	tgs, err := disc.refresh(t.Context())
	require.NoError(t, err)
	require.Len(t, tgs, 2)
	require.NotContains(t, tgs[0].Labels, model.LabelName(ecLabelTag+"Environment"),
		"the failed lookup must not label its cluster")
	require.Equal(t, model.LabelValue("staging"), tgs[1].Labels[ecLabelTag+"Environment"],
		"the other cluster must keep its tags")
}

func BenchmarkElasticacheRefresh(b *testing.B) {
	benchmarks := []struct {
		name     string
		clusters int
		nodes    int
	}{
		{"1Cluster/1Node", 1, 1},
		{"8Clusters/1Node", 8, 1},
		{"20Clusters/2Nodes", 20, 2},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			d := elasticacheTestDiscovery(b, elasticacheFixture(bm.clusters, bm.nodes))
			want := bm.clusters * bm.nodes

			b.ReportAllocs()
			for b.Loop() {
				tgs, err := d.refresh(b.Context())
				if err != nil {
					b.Fatal(err)
				}
				if got := len(tgs); got != want {
					b.Fatalf("got %d targets, want %d", got, want)
				}
			}
		})
	}
}

func BenchmarkElasticacheRefreshAPILatency(b *testing.B) {
	const (
		resourceCount = 20
		roundTrip     = time.Millisecond
	)

	data := elasticacheFixture(resourceCount, 1)
	d, api := elasticacheTestDiscoveryWithAPI(b, data)
	api.onRequest = func() { time.Sleep(roundTrip) }

	b.ReportAllocs()
	for b.Loop() {
		tgs, err := d.refresh(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		if got := len(tgs); got != resourceCount {
			b.Fatalf("got %d targets, want %d", got, resourceCount)
		}
	}
}
