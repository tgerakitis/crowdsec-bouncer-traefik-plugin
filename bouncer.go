// Package crowdsec_bouncer_traefik_plugin implements a middleware that communicates with crowdsec.
// It can cache results to filesystem or redis, or even ask crowdsec for every requests.
package crowdsec_bouncer_traefik_plugin //nolint:revive,stylecheck

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"

	cache "github.com/maxlerebourg/crowdsec-bouncer-traefik-plugin/pkg/cache"
	captcha "github.com/maxlerebourg/crowdsec-bouncer-traefik-plugin/pkg/captcha"
	configuration "github.com/maxlerebourg/crowdsec-bouncer-traefik-plugin/pkg/configuration"
	ip "github.com/maxlerebourg/crowdsec-bouncer-traefik-plugin/pkg/ip"
	logger "github.com/maxlerebourg/crowdsec-bouncer-traefik-plugin/pkg/logger"
)

const (
	crowdsecLapiHeader      = "X-Api-Key"
	crowdsecCapiHeader      = "Authorization"
	crowdsecLapiRoute       = "v1/decisions"
	crowdsecLapiStreamRoute = "v1/decisions/stream"
	crowdsecCapiLogin       = "v2/watchers/login"
	crowdsecCapiStreamRoute = "v2/decisions/stream"
	cacheTimeoutKey         = "updated"
)

//nolint:gochecknoglobals
var (
	isStartup               = true
	isCrowdsecStreamHealthy = true
	ticker                  chan bool
)

// CreateConfig creates the default plugin configuration.
func CreateConfig() *configuration.Config {
	return configuration.New()
}

// Bouncer a Bouncer struct.
type Bouncer struct {
	next     http.Handler
	name     string
	template *template.Template

	enabled                bool
	crowdsecScheme         string
	crowdsecHost           string
	crowdsecKey            string
	crowdsecMode           string
	crowdsecMachineID      string
	crowdsecPassword       string
	crowdsecScenarios      []string
	updateInterval         int64
	defaultDecisionTimeout int64
	customHeader           string
	crowdsecStreamRoute    string
	crowdsecHeader         string
	clientPoolStrategy     *ip.PoolStrategy
	serverPoolStrategy     *ip.PoolStrategy
	httpClient             *http.Client
	cacheClient            *cache.Client
	captchaClient          *captcha.Client
}

// New creates the crowdsec bouncer plugin.
func New(ctx context.Context, next http.Handler, config *configuration.Config, name string) (http.Handler, error) {
	logger.Init(config.LogLevel)
	err := configuration.ValidateParams(config)
	if err != nil {
		logger.Error(fmt.Sprintf("New:validateParams %s", err.Error()))
		return nil, err
	}

	serverChecker, _ := ip.NewChecker(config.ForwardedHeadersTrustedIPs)
	clientChecker, _ := ip.NewChecker(config.ClientTrustedIPs)

	var tlsConfig *tls.Config
	crowdsecStreamRoute := ""
	crowdsecHeader := ""
	if config.CrowdsecMode == configuration.AloneMode {
		config.CrowdsecCapiMachineID, _ = configuration.GetVariable(config, "CrowdsecCapiMachineID")
		config.CrowdsecCapiPassword, _ = configuration.GetVariable(config, "CrowdsecCapiPassword")
		config.CrowdsecLapiHost = "api.crowdsec.net"
		config.CrowdsecLapiScheme = "https"
		config.UpdateIntervalSeconds = 7200
		crowdsecStreamRoute = crowdsecCapiStreamRoute
		crowdsecHeader = crowdsecCapiHeader
	} else {
		crowdsecStreamRoute = crowdsecLapiStreamRoute
		crowdsecHeader = crowdsecLapiHeader
		tlsConfig, err = configuration.GetTLSConfigCrowdsec(config)
		if err != nil {
			logger.Error(fmt.Sprintf("New:getTLSConfigCrowdsec fail to get tlsConfig %s", err.Error()))
			return nil, err
		}
		apiKey, errAPIKey := configuration.GetVariable(config, "CrowdsecLapiKey")
		if errAPIKey != nil && len(tlsConfig.Certificates) == 0 {
			logger.Error(fmt.Sprintf("New:crowdsecLapiKey fail to get CrowdsecLapiKey and no client certificate setup %s", errAPIKey.Error()))
			return nil, err
		}
		config.CrowdsecLapiKey = apiKey
	}

	bouncer := &Bouncer{
		next:     next,
		name:     name,
		template: template.New("CrowdsecBouncer").Delims("[[", "]]"),

		enabled:                config.Enabled,
		crowdsecMode:           config.CrowdsecMode,
		crowdsecScheme:         config.CrowdsecLapiScheme,
		crowdsecHost:           config.CrowdsecLapiHost,
		crowdsecKey:            config.CrowdsecLapiKey,
		crowdsecMachineID:      config.CrowdsecCapiMachineID,
		crowdsecPassword:       config.CrowdsecCapiPassword,
		crowdsecScenarios:      config.CrowdsecCapiScenarios,
		updateInterval:         config.UpdateIntervalSeconds,
		customHeader:           config.ForwardedHeadersCustomName,
		defaultDecisionTimeout: config.DefaultDecisionSeconds,
		crowdsecStreamRoute:    crowdsecStreamRoute,
		crowdsecHeader:         crowdsecHeader,
		serverPoolStrategy: &ip.PoolStrategy{
			Checker: serverChecker,
		},
		clientPoolStrategy: &ip.PoolStrategy{
			Checker: clientChecker,
		},
		httpClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:    10,
				IdleConnTimeout: 30 * time.Second,
				TLSClientConfig: tlsConfig,
			},
			Timeout: time.Duration(config.HTTPTimeoutSeconds) * time.Second,
		},
		cacheClient:   &cache.Client{},
		captchaClient: &captcha.Client{},
	}
	config.RedisCachePassword, _ = configuration.GetVariable(config, "RedisCachePassword")
	bouncer.cacheClient.New(
		config.RedisCacheEnabled,
		config.RedisCacheHost,
		config.RedisCachePassword,
		config.RedisCacheDatabase,
	)
	if err := bouncer.captchaClient.New(
		config.CaptchaSiteKey,
		config.CaptchaSecretKey,
		config.CaptchaProvider,
		config.CaptchaTemplate,
		config.CaptchaTemplateFile,
		config.FallbackRemediation,
		config.CaptchaGracePeriod,
	); err != nil {
		logger.Info(fmt.Sprintf("New:captchaClient %s", err.Error()))
	}

	bouncer.captchaClient.Cache = bouncer.cacheClient //Reuse the same cache client for captcha

	if (config.CrowdsecMode == configuration.StreamMode || config.CrowdsecMode == configuration.AloneMode) && ticker == nil {
		if config.CrowdsecMode == configuration.AloneMode {
			if err := getToken(bouncer); err != nil {
				logger.Error(fmt.Sprintf("New:getToken %s", err.Error()))
				return nil, err
			}
		}
		handleStreamTicker(bouncer)
		isStartup = false
		ticker = startTicker(config, func() {
			handleStreamTicker(bouncer)
		})
	}
	logger.Debug(fmt.Sprintf("New initialized mode:%s", config.CrowdsecMode))

	return bouncer, nil
}

// ServeHTTP principal function of plugin.
//
//nolint:nestif
func (bouncer *Bouncer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if !bouncer.enabled {
		bouncer.next.ServeHTTP(rw, req)
		return
	}

	// Here we check for the trusted IPs in the customHeader
	remoteIP, err := ip.GetRemoteIP(req, bouncer.serverPoolStrategy, bouncer.customHeader)
	if err != nil {
		logger.Error(fmt.Sprintf("ServeHTTP:getRemoteIp ip:%s %s", remoteIP, err.Error()))
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	isTrusted, err := bouncer.clientPoolStrategy.Checker.Contains(remoteIP)
	if err != nil {
		logger.Error(fmt.Sprintf("ServeHTTP:checkerContains ip:%s %s", remoteIP, err.Error()))
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	// if our IP is in the trusted list we bypass the next checks
	logger.Debug(fmt.Sprintf("ServeHTTP ip:%s isTrusted:%v", remoteIP, isTrusted))
	if isTrusted {
		bouncer.next.ServeHTTP(rw, req)
		return
	}

	// TODO This should be simplified
	if bouncer.crowdsecMode != configuration.NoneMode {
		remediation, cacheErr := bouncer.cacheClient.Get(remoteIP)
		if cacheErr != nil {
			errString := cacheErr.Error()
			logger.Debug(fmt.Sprintf("ServeHTTP:getDecision ip:%s isBanned:false %s", remoteIP, errString))
			if errString != cache.CacheMiss {
				logger.Error(fmt.Sprintf("ServeHTTP:getDecision ip:%s %s", remoteIP, errString))
				rw.WriteHeader(http.StatusForbidden)
				return
			}
		} else {
			handleRemediation(remoteIP, remediation, bouncer, rw, req)
			return
		}
	}

	// Right here if we cannot join the stream we forbid the request to go on.
	if bouncer.crowdsecMode == configuration.StreamMode || bouncer.crowdsecMode == configuration.AloneMode {
		if isCrowdsecStreamHealthy {
			bouncer.next.ServeHTTP(rw, req)
		} else {
			logger.Debug(fmt.Sprintf("ServeHTTP isCrowdsecStreamHealthy:false ip:%s", remoteIP))
			rw.WriteHeader(http.StatusForbidden)
		}
	} else {
		//TODO handle error
		remediation, _ := handleNoStreamCache(bouncer, remoteIP)
		handleRemediation(remoteIP, remediation, bouncer, rw, req)
	}
}

// CUSTOM CODE.
// TODO place in another file.

// Decision Body returned from Crowdsec LAPI.
type Decision struct {
	ID        int    `json:"id"`
	Origin    string `json:"origin"`
	Type      string `json:"type"`
	Scope     string `json:"scope"`
	Value     string `json:"value"`
	Duration  string `json:"duration"`
	Scenario  string `json:"scenario"`
	Simulated bool   `json:"simulated"`
}

// Stream Body returned from Crowdsec Stream LAPI.
type Stream struct {
	Deleted []Decision `json:"deleted"`
	New     []Decision `json:"new"`
}

// Login Body returned from Crowdsec Login CAPI.
type Login struct {
	Code   int    `json:"code"`
	Token  string `json:"token"`
	Expire string `json:"expire"`
}

func handleRemediation(remoteIP, remediation string, bouncer *Bouncer, rw http.ResponseWriter, req *http.Request) {
	if remediation == cache.CaptchaValue && !bouncer.captchaClient.Valid && bouncer.captchaClient.FallbackRemediation == "ban" {
		logger.Debug("captcha is not valid and fallback remediation is set to ban, returning 403")
		remediation = cache.BannedValue
	}
	switch remediation {
	case cache.BannedValue:
		rw.WriteHeader(http.StatusForbidden)
		return
	case cache.CaptchaValue:
		logger.Debug(fmt.Sprintf("handleRemediation ip:%s remediation:captcha", remoteIP))
		//Check if request path is not favicon
		if !bouncer.captchaClient.CheckCookie(rw, req) && req.URL.Path != "/favicon.ico" {
			bouncer.captchaClient.ServeHTTP(rw, req, remoteIP)
			return
		}
	}
	logger.Debug(fmt.Sprintf("handleRemediation ip:%s remediation:pass", remoteIP))
	bouncer.next.ServeHTTP(rw, req)
}

func handleStreamTicker(bouncer *Bouncer) {
	if err := handleStreamCache(bouncer); err != nil {
		isCrowdsecStreamHealthy = false
		logger.Error(err.Error())
	} else {
		isCrowdsecStreamHealthy = true
	}
}

func startTicker(config *configuration.Config, work func()) chan bool {
	ticker := time.NewTicker(time.Duration(config.UpdateIntervalSeconds) * time.Second)
	stop := make(chan bool, 1)
	go func() {
		defer logger.Debug("ticker:stopped")
		for {
			select {
			case <-ticker.C:
				go work()
			case <-stop:
				return
			}
		}
	}()
	return stop
}

// We are now in none or live mode.
func handleNoStreamCache(bouncer *Bouncer, remoteIP string) (string, error) {
	value := cache.NoBannedValue
	isLiveMode := bouncer.crowdsecMode == configuration.LiveMode
	routeURL := url.URL{
		Scheme:   bouncer.crowdsecScheme,
		Host:     bouncer.crowdsecHost,
		Path:     crowdsecLapiRoute,
		RawQuery: fmt.Sprintf("ip=%v&banned=true", remoteIP),
	}
	body, err := crowdsecQuery(bouncer, routeURL.String(), false)
	if err != nil {
		return value, err
	}

	if bytes.Equal(body, []byte("null")) {
		if isLiveMode {
			bouncer.cacheClient.Set(remoteIP, value, bouncer.defaultDecisionTimeout)
		}
		return value, nil
	}

	var decisions []Decision
	err = json.Unmarshal(body, &decisions)
	if err != nil {
		return value, fmt.Errorf("handleNoStreamCache:parseBody %w", err)
	}
	if len(decisions) == 0 {
		if isLiveMode {
			bouncer.cacheClient.Set(remoteIP, value, bouncer.defaultDecisionTimeout)
		}
		return value, nil
	}
	var decision Decision
	for _, d := range decisions {
		decision = d
		if decision.Type == "ban" {
			break
		}
	}
	duration, err := time.ParseDuration(decision.Duration)
	if err != nil {
		return value, fmt.Errorf("handleNoStreamCache:parseDuration %w", err)
	}

	if isLiveMode {
		durationSecond := int64(duration.Seconds())
		if bouncer.defaultDecisionTimeout < durationSecond {
			durationSecond = bouncer.defaultDecisionTimeout
		}
		switch decision.Type {
		case "ban":
			value = cache.BannedValue
		case "captcha":
			value = cache.CaptchaValue
		default:
			logger.Debug(fmt.Sprintf("handleStreamCache:unknownType %s", decision.Type))
		}
		bouncer.cacheClient.Set(remoteIP, value, durationSecond)
	}
	return value, fmt.Errorf("handleNoStreamCache:banned")
}

func getToken(bouncer *Bouncer) error {
	loginURL := url.URL{
		Scheme: bouncer.crowdsecScheme,
		Host:   bouncer.crowdsecHost,
		Path:   crowdsecCapiLogin,
	}
	body, err := crowdsecQuery(bouncer, loginURL.String(), true)
	if err != nil {
		return err
	}
	var login Login
	err = json.Unmarshal(body, &login)
	if err != nil {
		isCrowdsecStreamHealthy = false
		return fmt.Errorf("getToken:parsingBody %w", err)
	}
	if login.Code == 200 && len(login.Token) > 0 {
		bouncer.crowdsecKey = login.Token
		logger.Debug(fmt.Sprintf("getToken statusCode:%d", login.Code))
		return nil
	}
	return fmt.Errorf("getToken statusCode:%d", login.Code)
}

func handleStreamCache(bouncer *Bouncer) error {
	// TODO clean properly on exit.
	// Instead of blocking the goroutine interval for all the secondary node,
	// if the master service is shut down, other goroutine can take the lead
	// because updated routine information is in the cache
	_, err := bouncer.cacheClient.Get(cacheTimeoutKey)
	if err == nil {
		logger.Debug("handleStreamCache:alreadyUpdated")
		return nil
	}
	if err.Error() != cache.CacheMiss {
		return err
	}
	bouncer.cacheClient.Set(cacheTimeoutKey, cache.NoBannedValue, bouncer.updateInterval-1)
	streamRouteURL := url.URL{
		Scheme:   bouncer.crowdsecScheme,
		Host:     bouncer.crowdsecHost,
		Path:     bouncer.crowdsecStreamRoute,
		RawQuery: fmt.Sprintf("startup=%t", !isCrowdsecStreamHealthy || isStartup),
	}
	body, err := crowdsecQuery(bouncer, streamRouteURL.String(), false)
	if err != nil {
		return err
	}
	var stream Stream
	err = json.Unmarshal(body, &stream)
	if err != nil {
		return fmt.Errorf("handleStreamCache:parsingBody %w", err)
	}
	for _, decision := range stream.New {
		duration, err := time.ParseDuration(decision.Duration)
		if err == nil {
			var value string
			switch decision.Type {
			case "ban":
				value = cache.BannedValue
			case "captcha":
				value = cache.CaptchaValue
			default:
				logger.Debug(fmt.Sprintf("handleStreamCache:unknownType %s", decision.Type))
			}
			bouncer.cacheClient.Set(decision.Value, value, int64(duration.Seconds()))
		}
	}
	for _, decision := range stream.Deleted {
		bouncer.cacheClient.Delete(decision.Value)
	}
	logger.Debug("handleStreamCache:updated")
	isCrowdsecStreamHealthy = true
	return nil
}

func crowdsecQuery(bouncer *Bouncer, stringURL string, isPost bool) ([]byte, error) {
	var req *http.Request
	if isPost {
		data := []byte(fmt.Sprintf(
			`{"machine_id": "%v","password": "%v","scenarios": ["%v"]}`,
			bouncer.crowdsecMachineID,
			bouncer.crowdsecPassword,
			strings.Join(bouncer.crowdsecScenarios, `","`),
		))
		req, _ = http.NewRequest(http.MethodPost, stringURL, bytes.NewBuffer(data))
	} else {
		req, _ = http.NewRequest(http.MethodGet, stringURL, nil)
	}
	req.Header.Add(bouncer.crowdsecHeader, bouncer.crowdsecKey)
	res, err := bouncer.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("crowdsecQuery url:%s %w", stringURL, err)
	}
	if res.StatusCode == http.StatusUnauthorized && bouncer.crowdsecMode == configuration.AloneMode {
		if errToken := getToken(bouncer); errToken != nil {
			return nil, fmt.Errorf("crowdsecQuery:renewToken url:%s %w", stringURL, errToken)
		}
		return crowdsecQuery(bouncer, stringURL, false)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crowdsecQuery url:%s, statusCode:%d", stringURL, res.StatusCode)
	}
	defer func() {
		if err = res.Body.Close(); err != nil {
			logger.Error(fmt.Sprintf("crowdsecQuery:closeBody %s", err.Error()))
		}
	}()
	body, err := io.ReadAll(res.Body)

	if err != nil {
		return nil, fmt.Errorf("crowdsecQuery:readBody %w", err)
	}
	return body, nil
}
