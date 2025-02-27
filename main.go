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

// Prometheus Service Discovery for AWS ElastiCache
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/refresh"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/documentation/examples/custom-sd/adapter"
	"github.com/prometheus/prometheus/util/strutil"
)

const (
	ecLabel                        = model.MetaLabelPrefix + "elasticache_"
	ecLabelCacheClusterID          = ecLabel + "cache_cluster_id"
	ecLabelCacheClusterStatus      = ecLabel + "cache_cluster_status"
	ecLabelCacheNodeID             = ecLabel + "cache_node_id"
	ecLabelCacheNodeStatus         = ecLabel + "cache_node_status"
	ecLabelCacheNodeType           = ecLabel + "cache_node_type"
	ecLabelCacheParameterGroupName = ecLabel + "cache_parameter_group_name"
	ecLabelCacheSubnetGroupName    = ecLabel + "cache_subnet_group_name"
	ecLabelCustomerAZ              = ecLabel + "customer_availability_zone"
	ecLabelEndpointAddress         = ecLabel + "endpoint_address"
	ecLabelEndpointPort            = ecLabel + "endpoint_port"
	ecLabelEngine                  = ecLabel + "engine"
	ecLabelEngineVersion           = ecLabel + "engine_version"
	ecLabelPreferredAZ             = ecLabel + "preferred_availability_zone"
	ecLabelReplicationGroupID      = ecLabel + "replication_group_id"
	ecLabelTag                     = ecLabel + "tag_"
)

type elasticacheMetrics struct {
	refreshMetrics discovery.RefreshMetricsInstantiator
}

var _ discovery.DiscovererMetrics = (*elasticacheMetrics)(nil)

// Register implements discovery.DiscovererMetrics.
func (m *elasticacheMetrics) Register() error {
	return nil
}

// Unregister implements discovery.DiscovererMetrics.
func (m *elasticacheMetrics) Unregister() {}

// ElasticacheSDConfig is the configuration for ElastiCache-based service discovery.
type ElasticacheSDConfig struct {
	Region                                  string
	AccessKey                               string
	SecretKey                               string
	Profile                                 string
	RoleARN                                 string
	cacheClusterID                          string
	showCacheClustersNotInReplicationGroups bool
	RefreshInterval                         time.Duration
}

// Name returns the name of the Elasticache Config.
func (*ElasticacheSDConfig) Name() string { return "elasticache" }

// NewDiscoverer returns a Discoverer for the Elasticache Config.
func (c *ElasticacheSDConfig) NewDiscoverer(opts discovery.DiscovererOptions) (discovery.Discoverer, error) {
	return NewElasticacheDiscovery(c, opts.Logger, opts.Metrics)
}

// NewDiscovererMetrics implements discovery.Config.
func (*ElasticacheSDConfig) NewDiscovererMetrics(_ prometheus.Registerer, rmi discovery.RefreshMetricsInstantiator) discovery.DiscovererMetrics {
	return &elasticacheMetrics{
		refreshMetrics: rmi,
	}
}

// ElasticacheDiscovery periodically performs Elasticache-SD requests. It implements
// the Prometheus Discoverer interface.
type ElasticacheDiscovery struct {
	*refresh.Discovery
	logger      *slog.Logger
	cfg         *ElasticacheSDConfig
	elasticache *elasticache.Client
	lasts       map[string]struct{}
	lastTags    map[string][]types.Tag
}

// NewElasticacheDiscovery returns a new ElasticacheDiscovery which periodically refreshes its targets.
func NewElasticacheDiscovery(conf *ElasticacheSDConfig, logger *slog.Logger, metrics discovery.DiscovererMetrics) (*ElasticacheDiscovery, error) {
	m, ok := metrics.(*elasticacheMetrics)
	if !ok {
		return nil, errors.New("invalid discovery metrics type")
	}

	if logger == nil {
		logger = promslog.NewNopLogger()
	}

	d := &ElasticacheDiscovery{
		logger: logger,
		cfg:    conf,
	}

	d.Discovery = refresh.NewDiscovery(
		refresh.Options{
			Logger:              logger,
			Mech:                "elasticache",
			Interval:            d.cfg.RefreshInterval,
			RefreshF:            d.refresh,
			MetricsInstantiator: m.refreshMetrics,
		},
	)

	return d, nil
}

func (d *ElasticacheDiscovery) elasticacheClient(ctx context.Context) (*elasticache.Client, error) {
	if d.elasticache != nil {
		return d.elasticache, nil
	}

	optFns := []func(*config.LoadOptions) error{
		config.WithRegion(d.cfg.Region),
		config.WithSharedConfigProfile(d.cfg.Profile),
	}

	if d.cfg.AccessKey != "" && d.cfg.SecretKey != "" {
		optFns = append(optFns, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(d.cfg.AccessKey, d.cfg.SecretKey, ""),
		))
	}

	cfg, err := config.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("could not load aws default config: %w", err)
	}

	if d.cfg.RoleARN != "" {
		sts := sts.NewFromConfig(cfg)
		creds := stscreds.NewAssumeRoleProvider(sts, d.cfg.RoleARN)

		cfg.Credentials = aws.NewCredentialsCache(creds)
	}

	d.elasticache = elasticache.NewFromConfig(cfg)

	return d.elasticache, nil
}

func (d *ElasticacheDiscovery) refresh(ctx context.Context) ([]*targetgroup.Group, error) {
	elasticacheClient, err := d.elasticacheClient(ctx)
	if err != nil {
		return nil, err
	}

	current := make(map[string]struct{})
	currentTags := make(map[string][]types.Tag)
	tgs := []*targetgroup.Group{}
	showCacheNodeInfo := true

	p := elasticache.NewDescribeCacheClustersPaginator(elasticacheClient, &elasticache.DescribeCacheClustersInput{
		CacheClusterId:                          &d.cfg.cacheClusterID,
		ShowCacheClustersNotInReplicationGroups: &d.cfg.showCacheClustersNotInReplicationGroups,
		ShowCacheNodeInfo:                       &showCacheNodeInfo,
	})

	for {
		o, err := p.NextPage(ctx)
		if err != nil {
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "AuthFailure" || apiErr.ErrorCode() == "UnauthorizedOperation") {
				d.elasticache = nil
			}

			return nil, fmt.Errorf("could not describe cache clusters: %w", err)
		}

		for _, cc := range o.CacheClusters {
			labels := model.LabelSet{
				model.LabelName(ecLabelCacheClusterID):          model.LabelValue(*cc.CacheClusterId),
				model.LabelName(ecLabelCacheClusterStatus):      model.LabelValue(*cc.CacheClusterStatus),
				model.LabelName(ecLabelCacheNodeType):           model.LabelValue(*cc.CacheNodeType),
				model.LabelName(ecLabelCacheParameterGroupName): model.LabelValue(*cc.CacheParameterGroup.CacheParameterGroupName),
				model.LabelName(ecLabelCacheSubnetGroupName):    model.LabelValue(*cc.CacheSubnetGroupName),
				model.LabelName(ecLabelEngine):                  model.LabelValue(*cc.Engine),
				model.LabelName(ecLabelEngineVersion):           model.LabelValue(*cc.EngineVersion),
				model.LabelName(ecLabelPreferredAZ):             model.LabelValue(*cc.PreferredAvailabilityZone),
			}

			if cc.ReplicationGroupId != nil {
				labels[model.LabelName(ecLabelReplicationGroupID)] = model.LabelValue(*cc.ReplicationGroupId)
			}

			tags := []types.Tag{}

			to, err := elasticacheClient.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{
				ResourceName: cc.ARN,
			})
			if err != nil {
				d.logger.Warn("could not list tags", "err", err.Error(), "ARN", *cc.ARN, "status", *cc.CacheClusterStatus)

				// If a cache cluster is not in "available" status (e.g. "snapshotting"),
				// its tags are unavailable, so if the relabeling logic depends on `__meta_elasticache_tag_<tagkey>` labels,
				// the clusters may disappear from the Prometheus targets when that happens,
				// thus we try to reuse the last tags we know about.
				if _, ok := d.lastTags[*cc.ARN]; ok {
					d.logger.Warn("reusing last known tags", "ARN", *cc.ARN)
					tags = d.lastTags[*cc.ARN]
				}
			} else {
				tags = to.TagList
			}

			currentTags[*cc.ARN] = tags

			for _, t := range tags {
				name := strutil.SanitizeLabelName(*t.Key)
				labels[ecLabelTag+model.LabelName(name)] = model.LabelValue(*t.Value)
			}

			for _, cn := range cc.CacheNodes {
				nodeLabels := labels.Clone()
				nodeLabels[model.LabelName(ecLabelCacheNodeID)] = model.LabelValue(*cn.CacheNodeId)
				nodeLabels[model.LabelName(ecLabelCacheNodeStatus)] = model.LabelValue(*cn.CacheNodeStatus)
				nodeLabels[model.LabelName(ecLabelCustomerAZ)] = model.LabelValue(*cn.CustomerAvailabilityZone)
				nodeLabels[model.LabelName(ecLabelEndpointAddress)] = model.LabelValue(*cn.Endpoint.Address)
				nodeLabels[model.LabelName(ecLabelEndpointPort)] = model.LabelValue(strconv.Itoa(int(*cn.Endpoint.Port)))

				// Placeholder address
				nodeLabels[model.AddressLabel] = model.LabelValue("undefined")

				source := fmt.Sprintf("%s/%s", *cc.ARN, *cn.CacheNodeId)
				d.logger.Debug("target added", "source", source)

				current[source] = struct{}{}

				tgs = append(tgs, &targetgroup.Group{
					Source: source,
					Labels: nodeLabels,
					Targets: []model.LabelSet{
						{model.AddressLabel: model.LabelValue("undefined")},
					},
				})
			}
		}

		if !p.HasMorePages() {
			break
		}
	}

	// Add empty groups for target which have been removed since the last refresh.
	for k := range d.lasts {
		if _, ok := current[k]; !ok {
			d.logger.Debug("target deleted", "source", k)

			tgs = append(tgs, &targetgroup.Group{Source: k})
		}
	}

	d.lasts = current
	d.lastTags = currentTags

	return tgs, nil
}

func main() {
	var (
		awsRegion                                 = kingpin.Flag("aws.region", "The AWS region. If not provided, the region from the default AWS credential chain is used.").String()
		awsAccessKey                              = kingpin.Flag("aws.access-key", "The AWS Access Key. Must be provided with --aws.secret-key. If not provided, the credentials from the default AWS credential chain are used.").String()
		awsSecretKey                              = kingpin.Flag("aws.secret-key", "The AWS Secret Key. Must be provided with --aws.access-key. If not provided, the credentials from the default AWS credential chain are used.").String()
		awsProfile                                = kingpin.Flag("aws.profile", "Named AWS profile used to connect to the API.").String()
		awsRoleARN                                = kingpin.Flag("aws.role-arn", "AWS Role ARN, an alternative to using AWS API keys.").String()
		ecCacheClusterID                          = kingpin.Flag("elasticache.cache-cluster-id", "The user-supplied cluster identifier. If this parameter is specified, only information about that specific cluster is returned. This parameter isn't case sensitive.").String()
		ecShowCacheClustersNotInReplicationGroups = kingpin.Flag("elasticache.show-cache-clusters-not-in-replication-groups", "An optional flag that can be included in the DescribeCacheCluster request to show only nodes (API/CLI: clusters) that are not members of a replication group. In practice, this means single node Redis clusters.").Bool()
		targetRefreshInterval                     = kingpin.Flag("target.refresh-interval", "Refresh interval to re-read the cluster list.").Default("60s").Duration()
		outputFile                                = kingpin.Flag("output.file", "The output filename for file_sd compatible file.").Default("elasticache.json").String()
		webConfig                                 = webflag.AddFlags(kingpin.CommandLine, ":8888")
		metricsPath                               = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
	)

	promslogConfig := &promslog.Config{}

	flag.AddFlags(kingpin.CommandLine, promslogConfig)
	kingpin.Version(version.Print("prometheus-elasticache-sd"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := promslog.New(promslogConfig)

	logger.Info("Starting prometheus-elasticache-sd", "version", version.Info())
	logger.Info("Build context", "context", version.BuildContext())

	conf := &ElasticacheSDConfig{
		Region:                                  *awsRegion,
		AccessKey:                               *awsAccessKey,
		SecretKey:                               *awsSecretKey,
		Profile:                                 *awsProfile,
		RoleARN:                                 *awsRoleARN,
		cacheClusterID:                          *ecCacheClusterID,
		showCacheClustersNotInReplicationGroups: *ecShowCacheClustersNotInReplicationGroups,
		RefreshInterval:                         *targetRefreshInterval,
	}
	discovery.RegisterConfig(conf)

	reg := prometheus.NewRegistry()
	refreshMetrics := discovery.NewRefreshMetrics(reg)
	metrics, err := discovery.RegisterSDMetrics(reg, refreshMetrics)
	if err != nil {
		logger.Error("failed to register service discovery metrics", "err", err)
		os.Exit(1)
	}

	discMetrics, ok := metrics[conf.Name()]
	if !ok {
		logger.Error("discoverer metrics not registered")
		os.Exit(1)
	}

	disc, err := conf.NewDiscoverer(discovery.DiscovererOptions{
		Logger:  logger,
		Metrics: discMetrics,
	})
	if err != nil {
		logger.Error("failed to instantiate discoverer", "err", err)
		os.Exit(1)
	}
	ctx := context.Background()

	sdAdapter := adapter.NewAdapter(ctx, *outputFile, "elasticache_sd", disc, logger, metrics, reg)
	sdAdapter.Run()

	prometheus.MustRegister(versioncollector.NewCollector("prometheus_elasticache_sd"))

	http.Handle(*metricsPath, promhttp.Handler())
	landingPage, err := web.NewLandingPage(web.LandingConfig{
		Name:        "ElastiCache Service Discovery",
		Description: "Prometheus ElastiCache Service Discovery",
		Version:     version.Info(),
		Links: []web.LandingLinks{
			{
				Address: *metricsPath,
				Text:    "Metrics",
			},
		},
	})
	if err != nil {
		logger.Error("Error instantiating landing page", "err", err)
		os.Exit(1)
	}
	http.Handle("/", landingPage)

	srv := &http.Server{}

	if err := web.ListenAndServe(srv, webConfig, logger); err != nil {
		logger.Error("Error starting HTTP server", "err", err)
		os.Exit(1)
	}
}
