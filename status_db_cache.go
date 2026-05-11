package main

import (
	"time"

	"github.com/patrickmn/go-cache"
)

// Segundos de cache do QueryRow em GetStatus (proxy/s3/hmac). Definir 0 desliga o cache no init.
const getStatusDBRowCacheSeconds = 45

// Snapshot de linha users usada só para montar proxy_config / s3_config / hmac em GetStatus.
type getStatusDBRowCached struct {
	ProxyURL        string
	S3Enabled       bool
	S3Endpoint      string
	S3Region        string
	S3Bucket        string
	S3PathStyle     bool
	S3PublicURL     string
	S3MediaDelivery string
	S3RetentionDays int
	HmacConfigured  bool
}

var getStatusRowCache *cache.Cache

func init() {
	if getStatusDBRowCacheSeconds > 0 {
		d := time.Duration(getStatusDBRowCacheSeconds) * time.Second
		getStatusRowCache = cache.New(d, d*2)
	}
}
