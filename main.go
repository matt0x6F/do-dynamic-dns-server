package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/godo"
	tld "github.com/jpillora/go-tld"
)

const (
	CheckIPURL = "https://checkip.amazonaws.com/"
)

var server *DDNSUpdater

func main() {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		log.Printf("failed to load config: %s", err)
	}

	if cfg.Debug {
		go func() {
			runtime.SetBlockProfileRate(1)
			runtime.SetMutexProfileFraction(1)
			log.Printf("Debug mode enabled, server running at: http://localhost:6060/debug/pprof/")
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	server := NewDDNSUpdater(cfg.Domains, cfg.Interval, cfg.DOToken)

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt)

	go func() {
		err := server.Run()
		if err != nil {
			log.Printf("error: %v\n", err)

			os.Exit(1)
		}
	}()

	log.Print("Server started")

	<-done

	log.Print("Signal received, stopping server")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		// extra handling here
		cancel()
	}()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %+v", err)
	}

	log.Print("Server exited properly")
}

func LoadConfigFromEnv() (*Config, error) {
	cfg := new(Config)

	cfg.DOToken = os.Getenv("DDNS_DO_API_TOKEN")
	interval, err := time.ParseDuration(os.Getenv("DDNS_INTERVAL"))
	if err != nil {
		return nil, fmt.Errorf("unable to parse DDNS_INTERVAL: %w", err)
	}

	cfg.Interval = interval
	domains := []string{}

	rawDomains := os.Getenv("DDNS_DOMAINS")

	parts := strings.Split(rawDomains, ",")
	domains = append(domains, parts...)

	if domains == nil {
		return nil, fmt.Errorf("DDNS_DOMAINS is required")
	}

	cfg.Domains = domains
	cfg.Debug, _ = strconv.ParseBool(os.Getenv("DDNS_DEBUG"))

	return cfg, nil
}

type Config struct {
	DOToken string
	// Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".
	Interval time.Duration
	// Comma separated list of domains to update.
	Domains []string
	Debug   bool
}

// NewDDNSUpdater creates a new DDNS updater
func NewDDNSUpdater(domains []string, interval time.Duration, token string) *DDNSUpdater {
	doClient := godo.NewFromToken(token)

	domainTable := make(map[string]godo.DomainRecord, len(domains))

	for _, domain := range domains {
		// these records get filled during synchronization
		domainTable[domain] = godo.DomainRecord{}
	}

	return &DDNSUpdater{
		httpClient: http.Client{Timeout: 2 * time.Second},
		doClient:   doClient,
		interval:   interval,
		recordMap:  domainTable,
		nextCheck:  time.Now(),
	}
}

type DDNSUpdater struct {
	httpClient http.Client
	doClient   *godo.Client
	// domain: address
	recordMap map[string]godo.DomainRecord
	interval  time.Duration
	lastSet   time.Time
	nextCheck time.Time
	currentIP net.IP
	shutdown  bool
	complete  bool
}

// Shutdown signals the Run method to shut down.
func (d *DDNSUpdater) Shutdown(ctx context.Context) error {
	// signal the run loop to exit
	d.shutdown = true

	// wait for the run loop to exit
	for _ = range time.Tick(1 * time.Second) {
		deadline, ok := ctx.Deadline()

		if ok && (time.Now().After(deadline) || time.Now().Equal(deadline)) {
			return fmt.Errorf("shutdown timeout reached")
		}

		if d.complete {
			break
		}
	}

	return nil
}

// syncRecords performs an initial synchronization of DigitalOcean DNS records to the local cache.
func (d *DDNSUpdater) syncRecords() error {
	log.Printf("Syncing %d records", len(d.recordMap))

	for name, _ := range d.recordMap {
		// this http:// thing is kind of hacky, but hostname.Parse() doesn't work without it
		hostname, err := tld.Parse("http://" + name)
		if err != nil {
			log.Printf("unable to parse domain (%s): %s", name, err)

			continue
		}

		domain := hostname.Domain + "." + hostname.TLD
		subdomain := hostname.Subdomain
		dnsName := subdomain + "." + domain
		// fixes root domains (@)
		dnsName = strings.TrimPrefix(dnsName, ".")

		log.Printf("searching record domain=%s name=%s original=%s", domain, dnsName, name)

		records, resp, err := d.doClient.Domains.RecordsByTypeAndName(context.TODO(), domain, "A", dnsName, nil)
		if err != nil {
			log.Printf("unable to fetch records. domain=%s subdomain=%s name=%s: %s", domain, subdomain, dnsName, err)

			continue
		}

		defer resp.Body.Close()

		if len(records) == 0 {
			log.Printf("no records found for domain=%s subdomain=%s name=%s", domain, subdomain, dnsName)

			continue
		}

		record := records[0]
		d.recordMap[name] = record
	}

	return nil
}

// Run should be run in a go routine. It runs in a loop.
func (d *DDNSUpdater) Run() error {
	err := d.syncRecords()
	if err != nil {
		return fmt.Errorf("unable to sync records: %s", err)
	}

	// use a one second loop so we can capture shutdowns
	for tick := range time.Tick(1 * time.Second) {
		now := time.Now()

		if d.shutdown {
			d.complete = true
			break
		}

		if d.nextCheck.Before(now) || d.nextCheck.Equal(now) {
			address, err := d.CheckIP()
			if err != nil {
				log.Printf("%s", err)
			}

			ip := net.ParseIP(strings.TrimSpace(address))

			log.Printf("ip=%s ts=%s", ip.String(), tick.String())

			if !d.currentIP.Equal(ip) {
				d.updateRecords(ip, tick)
			} else {
				log.Printf("ip is unchanged")
			}

			d.nextCheck = now.Add(d.interval)

			log.Printf("Next check at %s", d.nextCheck.Format(time.RFC3339))
		}
	}

	return nil
}

func (d *DDNSUpdater) CheckIP() (string, error) {
	req, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, CheckIPURL, nil)
	if err != nil {
		return "", fmt.Errorf("error while forming request: %v", err)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error while unpacking response: %v", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error while reading response body: \"%v\"", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("error from server (%d) body: \"%s\"", resp.StatusCode, body)
	}

	return strings.TrimSpace(string(body)), nil
}

// updateRecords updates records in digital ocean
func (d *DDNSUpdater) updateRecords(ip net.IP, ts time.Time) {
	oldIP := d.currentIP
	d.currentIP = ip

	log.Printf("ip changed to %s from %s", ip.String(), oldIP.String())

	for name, record := range d.recordMap {
		if record.Data == d.currentIP.String() {
			log.Printf("record consistent, skipping update")

			continue
		}

		// this http:// thing is kind of hacky, but hostname.Parse() doesn't work without it
		hostname, err := tld.Parse("http://" + name)
		if err != nil {
			log.Printf("unable to parse domain (%s): %s", name, err)

			continue
		}

		domain := hostname.Domain + "." + hostname.TLD

		r, resp, err := d.doClient.Domains.EditRecord(context.TODO(), domain, record.ID, &godo.DomainRecordEditRequest{
			Data: d.currentIP.String(),
		})
		if err != nil {
			log.Printf("error while updating domain record: %v", err)

			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("error while reading response body: \"%v\"", err)
			continue
		}

		if resp.StatusCode >= http.StatusBadRequest {
			log.Printf("error from DO api (%d) body: \"%s\"", resp.StatusCode, body)
			continue
		}

		log.Printf("updated record for domain=%s name=%s", domain, record.Name)

		d.recordMap[domain] = *r
	}

	d.lastSet = ts
}
