package main

//go:generate go run genversion.go

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/sirupsen/logrus"
)

const defaultIPService = "https://ip.shee.sh/"

var (
	log         *logrus.Entry
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
		log.Error("error when responding with alive", err)
	}
}

func isReady(w http.ResponseWriter, r *http.Request) {
	_, err := fmt.Fprint(w, "Ready.")
	if err != nil {
		log.Error("error when responding with ready", err)
	}
}

func setupLogger(debug, nojson bool) *logrus.Entry {
	log := logrus.New()
	if debug {
		log.SetLevel(logrus.DebugLevel)
	}
	var logger *logrus.Entry
	if nojson {
		logger = log.WithFields(logrus.Fields{})
	} else {
		log.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: time.RFC3339Nano,
			FieldMap: logrus.FieldMap{
				logrus.FieldKeyTime:  "@timestamp",
				logrus.FieldKeyLevel: "level",
				logrus.FieldKeyMsg:   "message",
				logrus.FieldKeyFunc:  "caller",
			},
		})
		log.SetOutput(os.Stdout)
		log.SetReportCaller(true)
		logger = log.WithFields(logrus.Fields{})
	}

	return logger
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
			log.Errorf("Failed to create DNS record: %s", err)
			return err
		}
		log.Infof("Created a new A record %s with IP %s", config.Host, ip)
		updateCount.Inc()
		return nil
	case 1:
		if records[0].Content == ip {
			log.Debugf("Host %s already has IP %s, not updating", config.Host, ip)
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
		log.Infof("Host %s IP successfully changed from %s to %s", config.Host, oldip, ip)
		updateCount.Inc()
		return nil
	default:
		log.Errorf("Name %s has %d DNS records - only a single record is supported", config.Host, len(records))
		return err
	}
}

func updateHostLoop(config CFUpdateConfig, sleep time.Duration) {
	go func() {
		for {
			log.Debugf("Starting update of host %s", config.Host)
			ip, err := getIP(config.IPService)
			if err != nil {
				log.Errorf("Failed to get IP: %s", err)
			}
			log.Debugf("Got IP %s", ip)
			err = updateHost(config, ip)
			if err != nil {
				log.Errorf("Failed to update DNS: %s", err)
			}
			log.Debugf("Finished update of host %s, sleeping %s", config.Host, sleep)
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

	logger := setupLogger(*debug, *noJSON)
	log = logger

	if sleepwarning != "" {
		logger.Warnf("Environment setting '%s' for sleep interval is not a positive integer, using default %d", sleepwarning, sleepdefault)
	}

	if len(*urlprefix) > 0 && (*urlprefix)[0] != '/' {
		logger.Fatalf("URL prefix must start with a / or it won't match (got %s)", *urlprefix)
	}
	if *zone == "" {
		logger.Fatal("Zone name must be set, set -zone or CFDNSUPDATER_ZONE")
	}
	if *host == "" {
		logger.Fatal("Host name must be set, set -host or CFDNSUPDATER_HOST")
	}
	if !strings.HasSuffix(*host, *zone) {
		logger.Fatal("The host name must end with the zone name")
	}
	if *email == "" {
		logger.Fatal("Cloudflare email must be set, set -email or CLOUDFLARE_EMAIL")
	}
	if *apiKey == "" {
		logger.Fatal("Host name must be set, set -api-key or CLOUDFLARE_API_KEY")
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
	log.Infof("cfdnsupdater %s [%s] listening on %s", Version, Commit, *listen)
	log.Fatal(http.ListenAndServe(*listen, nil))
}
