// Copyright 2023 LY Corporation
//
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

package token

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime/metrics"
	"strings"
	"time"

	"github.com/AthenZ/athenz/clients/go/zts"
	athenz "github.com/AthenZ/athenz/libs/go/sia/util"
	"github.com/cenkalti/backoff"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/AthenZ/k8s-athenz-sia/v3/pkg/config"
	extutil "github.com/AthenZ/k8s-athenz-sia/v3/pkg/util"
	"github.com/AthenZ/k8s-athenz-sia/v3/third_party/log"
	"github.com/AthenZ/k8s-athenz-sia/v3/third_party/util"
)

type daemon struct {
	accessTokenCache TokenCache
	roleTokenCache   TokenCache

	// keyFile      string
	// certFile     string
	// serverCACert string
	// endpoint     string
	ztsClient *zts.ZTSClient
	saService string

	tokenRESTAPI        bool
	tokenType           mode
	tokenDir            string
	tokenRefresh        time.Duration
	tokenExpiryInSecond int
	roleAuthHeader      string
}

func newDaemon(idConfig *config.IdentityConfig, tt mode) (*daemon, error) {

	// initialize token cache with placeholder
	tokenExpiryInSecond := int(idConfig.TokenExpiry.Seconds())
	accessTokenCache := NewLockedTokenCache()
	roleTokenCache := NewLockedTokenCache()
	targets := strings.Split(idConfig.TargetDomainRoles, ",")
	if idConfig.TargetDomainRoles != "" || len(targets) != 1 {
		for _, dr := range targets {
			domain, role, err := athenz.SplitRoleName(dr)
			if err != nil {
				return nil, fmt.Errorf("Invalid TargetDomainRoles[%s]: %s", idConfig.TargetDomainRoles, err.Error())
			}
			if tt&mACCESS_TOKEN != 0 {
				accessTokenCache.Store(CacheKey{Domain: domain, Role: role, MaxExpiry: tokenExpiryInSecond}, &AccessToken{})
			}
			if tt&mROLE_TOKEN != 0 {
				roleTokenCache.Store(CacheKey{Domain: domain, Role: role, MinExpiry: tokenExpiryInSecond}, &RoleToken{})
			}
		}
	}

	// register prometheus metrics
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cached_token_bytes",
		Help: "Number of bytes cached.",
		ConstLabels: prometheus.Labels{
			"type": "accesstoken",
		},
	}, func() float64 {
		return float64(accessTokenCache.Size())
	})
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cached_token_bytes",
		Help: "Number of bytes cached.",
		ConstLabels: prometheus.Labels{
			"type": "roletoken",
		},
	}, func() float64 {
		return float64(roleTokenCache.Size())
	})
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cached_token_entries",
		Help: "Number of entries cached.",
		ConstLabels: prometheus.Labels{
			"type": "accesstoken",
		},
	}, func() float64 {
		return float64(accessTokenCache.Len())
	})
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cached_token_entries",
		Help: "Number of entries cached.",
		ConstLabels: prometheus.Labels{
			"type": "roletoken",
		},
	}, func() float64 {
		return float64(roleTokenCache.Len())
	})

	ztsClient, err := newZTSClient(idConfig.KeyFile, idConfig.CertFile, idConfig.ServerCACert, idConfig.Endpoint)
	if err != nil {
		return nil, err
	}

	saService := extutil.ServiceAccountToService(idConfig.ServiceAccount)
	if saService == "" {
		// TODO: get service from svc cert
		// https://github.com/AthenZ/athenz/blob/73b25572656f289cce501b4c2fe78f86656082e7/libs/go/athenzutils/principal.go
		// func ExtractServicePrincipal(x509Cert x509.Certificate) (string, error)
	}

	return &daemon{
		accessTokenCache: accessTokenCache,
		roleTokenCache:   roleTokenCache,

		ztsClient: ztsClient,
		saService: saService,

		tokenRESTAPI:        idConfig.TokenServerRESTAPI,
		tokenType:           tt,
		tokenDir:            idConfig.TokenDir,
		tokenRefresh:        idConfig.TokenRefresh,
		tokenExpiryInSecond: tokenExpiryInSecond,
		roleAuthHeader:      idConfig.RoleAuthHeader,
	}, nil
}

func (d *daemon) updateTokenWithRetry() error {
	// backoff config with first retry delay of 5s, and backoff retry until tokenRefresh / 4
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 5 * time.Second
	b.Multiplier = 2
	b.MaxElapsedTime = d.tokenRefresh / 4

	notifyOnErr := func(err error, backoffDelay time.Duration) {
		log.Errorf("Failed to refresh tokens: %s. Retrying in %s", err.Error(), backoffDelay)
	}
	return backoff.RetryNotify(d.updateToken, b, notifyOnErr)
}

func (d *daemon) updateToken() error {
	if err := d.fetchTokensAndUpdateCaches(); err != nil {
		log.Warnf("Error while requesting tokens: %s", err.Error())
		return err
	}

	return d.writeFiles()
}

func (d *daemon) writeFiles() error {
	if d.tokenDir == "" {
		log.Debugf("Skipping to write token files to directory[%s]", d.tokenDir)
		return nil
	}

	w := util.NewWriter()
	d.accessTokenCache.Range(func(k CacheKey, t Token) error {
		domain := t.Domain()
		role := t.Role()
		at := t.Raw()
		log.Infof("[New Access Token] Domain: %s, Role: %s", domain, role)
		outPath := filepath.Join(d.tokenDir, domain+":role."+role+".accesstoken")
		log.Debugf("Saving Access Token[%d bytes] at %s", len(at), outPath)
		if err := w.AddBytes(outPath, 0644, []byte(at)); err != nil {
			return errors.Wrap(err, "unable to save access token")
		}
		return nil
	})
	d.roleTokenCache.Range(func(k CacheKey, t Token) error {
		domain := t.Domain()
		role := t.Role()
		rt := t.Raw()
		log.Infof("[New Role Token] Domain: %s, Role: %s", domain, role)
		outPath := filepath.Join(d.tokenDir, domain+":role."+role+".roletoken")
		log.Debugf("Saving Role Token[%d bytes] at %s", len(rt), outPath)
		if err := w.AddBytes(outPath, 0644, []byte(rt)); err != nil {
			return errors.Wrap(err, "unable to save role token")
		}
		return nil
	})

	return w.Save()
}

// fetchTokensAndUpdateCaches fetches tokens by ZTS API calls, and then updates caches as a batch
func (d *daemon) fetchTokensAndUpdateCaches() error {

	atTargets := d.accessTokenCache.Keys()
	rtTargets := d.roleTokenCache.Keys()
	log.Infof("Attempting to fetch tokens from Athenz ZTS server: access token targets[%v], role token targets[%v]...", atTargets, rtTargets)

	// fetch tokens
	atUpdateOps := make([]func(), 0, len(atTargets))
	for _, t := range atTargets {
		key := t // prevent closure over loop variable
		at, err := fetchAccessToken(d.ztsClient, key, d.saService)
		if err != nil {
			return err
		}
		atUpdateOps = append(atUpdateOps, func() {
			d.accessTokenCache.Store(key, at)
			log.Debugf("Successfully received token from Athenz ZTS server: accessTokens(%s, len=%d)", key, len(at.Raw()))
		})
	}
	rtUpdateOps := make([]func(), 0, len(rtTargets))
	for _, t := range rtTargets {
		key := t // prevent closure over loop variable
		rt, err := fetchRoleToken(d.ztsClient, key)
		if err != nil {
			return err
		}
		rtUpdateOps = append(rtUpdateOps, func() {
			d.roleTokenCache.Store(key, rt)
			log.Debugf("Successfully received token from Athenz ZTS server: roleTokens(%s, len=%d)", key, len(rt.Raw()))
		})
	}

	// batch update caches
	for _, ops := range atUpdateOps {
		ops()
	}
	for _, ops := range rtUpdateOps {
		ops()
	}
	log.Infof("Successfully updated token cache: accessTokens(%d), roleTokens(%d)", len(atUpdateOps), len(rtUpdateOps))
	return nil
}

// Tokend starts the token server and refreshes tokens periodically.
func Tokend(idConfig *config.IdentityConfig, stopChan <-chan struct{}) (error, <-chan struct{}) {

	// validate
	if stopChan == nil {
		panic(fmt.Errorf("Tokend: stopChan cannot be empty"))
	}
	tt := newType(idConfig.TokenType)
	if idConfig.TokenServerAddr == "" || tt == 0 {
		log.Infof("Token server is disabled due to insufficient options: address[%s], roles[%s], token-type[%s]", idConfig.TokenServerAddr, idConfig.TargetDomainRoles, idConfig.TokenType)
		return nil, nil
	}

	d, err := newDaemon(idConfig, tt)
	if err != nil {
		return err, nil
	}

	// initialize
	err = d.updateTokenWithRetry()
	if err != nil {
		log.Errorf("Failed to get initial tokens after multiple retries: %s", err.Error())
	}
	if idConfig.Init {
		log.Infof("Token server is disabled for init mode: address[%s]", idConfig.TokenServerAddr)
		return nil, nil
	}

	// start token server daemon
	httpServer := &http.Server{
		Addr:      idConfig.TokenServerAddr,
		Handler:   newHandlerFunc(d, idConfig.TokenServerTimeout),
		TLSConfig: nil,
	}
	if idConfig.TokenServerTLSCertPath != "" && idConfig.TokenServerTLSKeyPath != "" {
		httpServer.TLSConfig, err = NewTLSConfig(idConfig.TokenServerTLSCAPath, idConfig.TokenServerTLSCertPath, idConfig.TokenServerTLSKeyPath)
		if err != nil {
			return err, nil
		}
	}
	serverDone := make(chan struct{}, 1)
	go func() {
		log.Infof("Starting token provider[%s]", idConfig.TokenServerAddr)
		listenAndServe := func() error {
			if httpServer.TLSConfig != nil {
				return httpServer.ListenAndServeTLS("", "")
			}
			return httpServer.ListenAndServe()
		}
		if err := listenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorf("Failed to start token provider: %s", err.Error())
		}
		close(serverDone)
	}()

	// start token refresh daemon
	shutdownChan := make(chan struct{}, 1)
	t := time.NewTicker(d.tokenRefresh)
	go func() {
		defer t.Stop()
		defer close(shutdownChan)

		for {
			log.Infof("Will refresh tokens for after %s", d.tokenRefresh.String())
			select {
			case <-t.C:
				err := d.updateTokenWithRetry()
				if err != nil {
					log.Errorf("Failed to refresh tokens after multiple retries: %s", err.Error())
				}
			case <-stopChan:
				log.Info("Initiating shutdown of token provider daemon ...")
				time.Sleep(idConfig.ShutdownDelay)
				ctx, cancel := context.WithTimeout(context.Background(), idConfig.ShutdownTimeout)
				defer cancel()
				httpServer.SetKeepAlivesEnabled(false)
				if err := httpServer.Shutdown(ctx); err != nil {
					log.Fatalf("Failed to shutdown token provider: %s", err.Error())
				}
				<-serverDone
				return
			}
		}
	}()

	// start token cache report daemon (no need for graceful shutdown)
	report := func() {
		// gather golang metrics
		const sysMemMetric = "/memory/classes/total:bytes"                  // go_memstats_sys_bytes
		const heapMemMetric = "/memory/classes/heap/objects:bytes"          // go_memstats_heap_alloc_bytes
		const releasedHeapMemMetric = "/memory/classes/heap/released:bytes" // go_memstats_heap_released_bytes
		// https://pkg.go.dev/runtime/metrics#pkg-examples
		// https://github.com/prometheus/client_golang/blob/3f8bd73e9b6d1e20e8e1536622bd0fda8bb3cb50/prometheus/go_collector_latest.go#L32
		samples := make([]metrics.Sample, 3)
		samples[0].Name = sysMemMetric
		samples[1].Name = heapMemMetric
		samples[2].Name = releasedHeapMemMetric
		metrics.Read(samples)
		validSample := func(s metrics.Sample) float64 {
			name, value := s.Name, s.Value
			switch value.Kind() {
			case metrics.KindUint64:
				return float64(value.Uint64())
			case metrics.KindFloat64:
				return value.Float64()
			case metrics.KindBad:
				// Check if the metric is actually supported. If it's not, the resulting value will always have kind KindBad.
				panic(fmt.Sprintf("%q: metric is no longer supported", name))
			default:
				// Check if the metrics specification has changed.
				panic(fmt.Sprintf("%q: unexpected metric Kind: %v\n", name, value.Kind()))
			}
		}
		sysMemValue := validSample(samples[0])
		heapMemValue := validSample(samples[1])
		releasedHeapMemValue := validSample(samples[2])
		sysMemInUse := sysMemValue - releasedHeapMemValue

		// gather token cache metrics
		atcSize := d.accessTokenCache.Size()
		atcLen := d.accessTokenCache.Len()
		rtcSize := d.roleTokenCache.Size()
		rtcLen := d.roleTokenCache.Len()
		totalSize := atcSize + rtcSize
		totalLen := atcLen + rtcLen

		// report as log message
		toMB := func(f float64) float64 {
			return f / 1024 / 1024
		}
		log.Infof("system_memory_inuse[%.1fMB]; go_memstats_heap_alloc_bytes[%.1fMB]; accesstoken:cached_token_bytes[%.1fMB],entries[%d]; roletoken:cached_token_bytes[%.1fMB],entries[%d]; total:cached_token_bytes[%.1fMB],entries[%d]; cache_token_ratio:sys[%.1f%%],heap[%.1f%%]", toMB(sysMemInUse), toMB(heapMemValue), toMB(float64(atcSize)), atcLen, toMB(float64(rtcSize)), rtcLen, toMB(float64(totalSize)), totalLen, float64(totalSize)/sysMemInUse*100, float64(totalSize)/heapMemValue*100)

		// TODO: memory triggers
		// if mem > warn threshold, warning log
		// if mem > error threshold, binary heap dump, i.e. debug.WriteHeapDump(os.Stdout.Fd())
	}
	reportTicker := time.NewTicker(time.Minute)
	go func() {
		defer reportTicker.Stop()
		for {
			select {
			case <-reportTicker.C:
				report()
			case <-stopChan:
				// stop token cache report daemon
				return
			}
		}
	}()

	return nil, shutdownChan
}
