package main

//go:generate go run genversion.go

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const defaultIPService = "https://ip.shee.sh/"

var (
	updateCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cfdnsupdater_update_count",
		Help: "The number of DNS updates completed",
	})
)

type CFUpdateConfig struct {
	Zone      string
	Host      string
	Email     string
	ApiKey    string
	IPService string
}

func isAlive(w http.ResponseWriter, r *http.Request) {
	_, err := fmt.Fprint(w, "Alive.")
	if err != nil {
		slog.Error("error when responding with alive", "error", err)
	}
}

func isReady(w http.ResponseWriter, r *http.Request) {
	_, err := fmt.Fprint(w, "Ready.")
	if err != nil {
		slog.Error("error when responding with ready", "error", err)
	}
}

func setupLogger(debug, nojson bool) {
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Key = "@timestamp"
			}
			return a
		},
	}
	if debug {
		opts.Level = slog.LevelDebug
	}

	var handler slog.Handler
	if nojson {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler).With(
		"service.name", "cfdnsupdater",
		"service.version", Version,
		"event.module", "cloudflare",
	))

	// logrus.FieldKeyTime:  "@timestamp",
	// logrus.FieldKeyLevel: "level",
	// logrus.FieldKeyMsg:   "message",
	// logrus.FieldKeyFunc:  "caller",
}

func getIP(ip_service string) (string, error) {
	dialer := net.Dialer{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr)
	}
	client := http.Client{
		Transport: transport,
	}
	req, err := http.NewRequest("GET", ip_service, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("cfdnsupdater/%s", Version))
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}

	if res.StatusCode != http.StatusOK {
		return "", errors.New(fmt.Sprintf("Unexpected HTTP status %s", res.Status))
	}

	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func updateHost(config CFUpdateConfig, ip string) error {
	api, err := cloudflare.New(config.ApiKey, config.Email)
	if err != nil {
		return err
	}

	ctx := context.Background()

	zoneID, err := api.ZoneIDByName(config.Zone)
	if err != nil {
		return err
	}
	zone := cloudflare.ZoneIdentifier(zoneID)

	hostrec := cloudflare.ListDNSRecordsParams{Name: config.Host, Type: "A"}

	records, _, err := api.ListDNSRecords(ctx, zone, hostrec)
	if err != nil {
		return err
	}

	switch len(records) {
	case 0:
		_, err := api.CreateDNSRecord(ctx, zone, cloudflare.CreateDNSRecordParams{
			Name:    config.Host,
			Type:    "A",
			Content: ip,
		})
		if err != nil {
			slog.Error("Failed to create DNS record", "error", err)
			return err
		}
		slog.Info("Created a new A record", "fqdn", config.Host, "ip", ip)
		updateCount.Inc()
		return nil
	case 1:
		if records[0].Content == ip {
			slog.Debug("IP is already correct", "fqdn", config.Host, "ip", ip)
			return nil
		}

		oldip := records[0].Content
		_, err = api.UpdateDNSRecord(ctx, zone, cloudflare.UpdateDNSRecordParams{
			ID:      records[0].ID,
			Content: ip,
		})
		if err != nil {
			return err
		}
		slog.Info("IP successfully changed",
			"dns.question.name", config.Host,
			"source.address", oldip,
			"destination.address", ip,
			"event.action", "ip_update",
			"event.dataset", "dns",
		)
		updateCount.Inc()
		return nil
	default:
		slog.Error(fmt.Sprintf("Name %s has %d DNS records - only a single record is supported", config.Host, len(records)))
		return err
	}
}

func updateHostLoop(config CFUpdateConfig, sleep time.Duration) {
	go func() {
		for {
			slog.Debug("Starting update of host", "fqdn", config.Host)
			ip, err := getIP(config.IPService)
			if err != nil {
				slog.Error("Failed to get IP", "error", err)
			}
			slog.Debug("Got IP", "ip", ip)
			err = updateHost(config, ip)
			if err != nil {
				slog.Error("Failed to update DNS", "error", err)
			}
			slog.Debug("Finished update, sleeping", "interval", sleep)
			time.Sleep(sleep)
		}
	}()
}

func main() {
	debug := flag.Bool("debug", false, "enable debug logging")
	noJSON := flag.Bool("no-json", false, "disable json logging")
	zone := flag.String("zone", os.Getenv("CFDNSUPDATER_ZONE"), "name of the zone to update")
	host := flag.String("host", os.Getenv("CFDNSUPDATER_HOST"), "FQDN of the host to update")
	email := flag.String("email", os.Getenv("CLOUDFLARE_EMAIL"), "Cloudflare account email address")
	apiKey := flag.String("api-key", os.Getenv("CLOUDFLARE_API_KEY"), "Cloudflare account API key")
	ipService := flag.String("ip-service", cmp.Or(os.Getenv("CFDNSUPDATER_IP_SERVICE"), defaultIPService), "The URL of a service which returns our current IP")
	listen := flag.String("listen", ":9876", "listen parameter")
	urlprefix := flag.String("urlprefix", "", "prefix for URL paths")
	showVersion := flag.Bool("version", false, "show version and exit")
	sleepdefault := uint(300)
	sleepwarning := ""
	if s := os.Getenv("CFDNSUPDATER_SLEEP_INTERVAL"); s != "" {
		si, err := strconv.ParseUint(s, 10, 0)
		if err != nil {
			// defer warning about incorrect setting until logger is set up
			sleepwarning = s
		} else {
			sleepdefault = uint(si)
		}
	}
	sleepinterval := flag.Uint("sleep-interval", sleepdefault, "period to sleep between runs (env: CFDNSUPDATER_SLEEP_INTERVAL)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("cfdnsupdater %s [%s]\n", Version, Commit)
		os.Exit(0)
	}

	setupLogger(*debug, *noJSON)

	if sleepwarning != "" {
		slog.Warn("Environment setting '%s' for sleep interval is not a positive integer, using default %d", sleepwarning, sleepdefault)
	}

	if len(*urlprefix) > 0 && (*urlprefix)[0] != '/' {
		slog.Error(fmt.Sprintf("URL prefix must start with a / or it won't match (got %s)", *urlprefix))
		os.Exit(1)
	}
	if *zone == "" {
		slog.Error("Zone name must be set, set -zone or CFDNSUPDATER_ZONE")
		os.Exit(1)
	}
	if *host == "" {
		slog.Error("Host name must be set, set -host or CFDNSUPDATER_HOST")
		os.Exit(1)
	}
	if !strings.HasSuffix(*host, *zone) {
		slog.Error("The host name must end with the zone name")
		os.Exit(1)
	}
	if *email == "" {
		slog.Error("Cloudflare email must be set, set -email or CLOUDFLARE_EMAIL")
		os.Exit(1)
	}
	if *apiKey == "" {
		slog.Error("Host name must be set, set -api-key or CLOUDFLARE_API_KEY")
		os.Exit(1)
	}

	updateHostLoop(CFUpdateConfig{
		Zone:      *zone,
		Host:      *host,
		Email:     *email,
		ApiKey:    *apiKey,
		IPService: *ipService,
	}, time.Duration(*sleepinterval)*time.Second)

	murl := *urlprefix + "/metrics"
	rurl := *urlprefix + "/ready"
	aurl := *urlprefix + "/alive"

	http.Handle(murl, promhttp.Handler())
	http.HandleFunc(rurl, isReady)
	http.HandleFunc(aurl, isAlive)
	slog.Info(fmt.Sprintf("cfdnsupdater %s [%s] listening on %s", Version, Commit, *listen))
	if err := http.ListenAndServe(*listen, nil); err != nil {
		slog.Error("Failed to start HTTP server", "error", err)
	}
}
