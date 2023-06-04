# Prometheus AWS ElastiCache Service Discovery

[![release](https://img.shields.io/github/v/release/maxbrunet/prometheus-elasticache-sd?sort=semver)](https://github.com/maxbrunet/prometheus-elasticache-sd/releases)
[![build](https://github.com/maxbrunet/prometheus-elasticache-sd/actions/workflows/build.yml/badge.svg)](https://github.com/maxbrunet/prometheus-elasticache-sd/actions/workflows/build.yml)
[![go report](https://goreportcard.com/badge/github.com/maxbrunet/prometheus-elasticache-sd)](https://goreportcard.com/report/github.com/maxbrunet/prometheus-elasticache-sd)

ElastiCache SD allows retrieving scrape targets from [AWS ElastiCache](https://aws.amazon.com/elasticache/) cache nodes for [Prometheus](https://prometheus.io/). **No address is defined by default**, it must be configured with relabeling and requires a [third-party exporter](https://prometheus.io/docs/instrumenting/exporters/#third-party-exporters) supporting the [multi-target pattern](https://prometheus.io/docs/guides/multi-target-exporter/).

## Configuration

Help on flags:

```
./prometheus-elasticache-sd --help
```

The following meta labels are available on targets during [relabeling](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#relabel_config):

* `__meta_elasticache_cache_cluster_id`: The identifier of the cluster.
* `__meta_elasticache_cache_cluster_status`: The current state of this cluster. See `CacheClusterStatus` in the [API Reference](https://docs.aws.amazon.com/AmazonElastiCache/latest/APIReference/API_CacheCluster.html) for possible values.
* `__meta_elasticache_cache_node_id`: The cache node identifier. A node ID is a numeric identifier (0001, 0002, etc.). The combination of cluster ID and node ID uniquely identifies every cache node used in a customer's Amazon account.
* `__meta_elasticache_cache_node_status`: The current state of this cache node. See `CacheNodeStatus` in the [API Reference](https://docs.aws.amazon.com/AmazonElastiCache/latest/APIReference/API_CacheNode.html) for possible values.
* `__meta_elasticache_cache_node_type`: The name of the compute and memory capacity node type for the cluster.
* `__meta_elasticache_cache_parameter_group_name`: The name of the cache parameter group.
* `__meta_elasticache_cache_subnet_group_name`: The name of the cache subnet group associated with the cluster.
* `__meta_elasticache_customer_availability_zone`: The Availability Zone where this node was created and now resides.
* `__meta_elasticache_endpoint_address`: The DNS hostname of the cache node.
* `__meta_elasticache_endpoint_port`: The port number that the cache engine is listening on.
* `__meta_elasticache_engine_version`: The version of the cache engine that is used in this cluster.
* `__meta_elasticache_engine`: The name of the cache engine (`memcached` or `redis`) used for this cluster.
* `__meta_elasticache_preferred_availability_zone`: The name of the Availability Zone in which the cluster is located or "Multiple" if the cache nodes are located in different Availability Zones.
* `__meta_elasticache_replication_group_id`: The replication group to which this cluster belongs. If this label is absent, the cluster is not associated with any replication group.
* `__meta_elasticache_tag_<tagkey>`: The tag's value.

The following AWS IAM permissions are required:

* `elasticache:DescribeCacheClusters`
* `elasticache:ListTagsForResource`

## Usage

### Docker

To run the ElastiCache SD as a Docker container, run:

```
docker run ghcr.io/maxbrunet/prometheus-elasticache-sd:latest --help
```

### oliver006/redis_exporter

This service discovery can be used with [oliver006/redis_exporter](https://github.com/oliver006/redis_exporter#prometheus-configuration-to-scrape-multiple-redis-hosts), here is a sample Prometheus configuration:

```yaml
scrape_configs:
  - job_name: "redis_exporter_targets"
    file_sd_configs:
    - files:
        - /path/to/elasticache.json  # Set file path with --output.file flag
    metrics_path: /scrape
    relabel_configs:
      # Filter for Redis cache nodes
      - source_labels: [__meta_elasticache_engine]
        regex: redis
        action: keep
      # Build Redis URL to use as target parameter for the exporter
      - source_labels:
          - __meta_elasticache_endpoint_address
          - __meta_elasticache_endpoint_port
        replacement: redis://$1
        separator: ':'
        target_label: __param_target
      # Use Redis URL as instance label
      - source_labels: [__param_target]
        target_label: instance
      # Set exporter address
      - target_label: __address__
        replacement: <<REDIS-EXPORTER-HOSTNAME>>:9121
```

### prometheus/memcached_exporter

This service discovery can be used with the official [memcached_exporter](https://github.com/prometheus/memcached_exporter),
see its [README](https://github.com/prometheus/memcached_exporter#multi-target) for details.

## Development

### Build

```
docker build -t prometheus-elasticache-sd .
```

### Test

```
go test -v ./...
```

## License

Apache License 2.0, see [LICENSE](LICENSE).
