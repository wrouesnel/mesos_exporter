package main

import (
	"flag"
	"log"
	"net/http"
	"crypto/tls"
	"crypto/x509"
	"os"
	"io/ioutil"
	"time"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var errorCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "mesos",
	Subsystem: "collector",
	Name:      "errors_total",
	Help:      "Total number of internal mesos-collector errors.",
})

func init() {
	prometheus.MustRegister(errorCounter)
}

func getX509CertPool(pemFiles []string) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, f := range pemFiles {
		content, err := ioutil.ReadFile(f)
		if err != nil {
			log.Fatal(err)
		}
		ok := pool.AppendCertsFromPEM(content)
		if !ok {
			log.Fatal("Error parsing .pem file %s", f)
		}
	}
	return pool
}

func mkHttpClient(url string, timeout time.Duration, auth authInfo, certPool *x509.CertPool, clientCert *tls.Certificate) *httpClient {
	tlsConfig := &tls.Config{RootCAs: certPool}
	if clientCert != nil {
		tlsConfig.Certificates = []tls.Certificate{*clientCert}
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	return &httpClient{
		http.Client{Timeout: timeout, Transport: transport},
		url,
		auth,
	}
}

func main() {
	fs := flag.NewFlagSet("mesos-exporter", flag.ExitOnError)
	addr := fs.String("addr", ":9110", "Address to listen on")
	masterURL := fs.String("master", "", "Expose metrics from master running on this URL")
	slaveURL := fs.String("slave", "", "Expose metrics from slave running on this URL")
	timeout := fs.Duration("timeout", 5*time.Second, "Master polling timeout")
	exportedTaskLabels := fs.String("exportedTaskLabels", "", "Comma-separated list of task labels to include in the task_labels metric")
	ignoreCompletedFrameworkTasks := fs.Bool("ignoreCompletedFrameworkTasks", false, "Don't export task_state_time metric");
	trustedCerts := fs.String("trustedCerts", "", "Comma-separated list of certificates (.pem files) trusted for requests to Mesos endpoints")
	clientCert := fs.String("clientCert", "", "client certificate public key to use for connecting to Mesos")
	clientKey := fs.String("clientKey", "", "client certificate private key to use for connecting to Mesos")

	fs.Parse(os.Args[1:])
	if *masterURL != "" && *slaveURL != "" {
		log.Fatal("Only -master or -slave can be given at a time")
	}

	auth := authInfo{
		os.Getenv("MESOS_EXPORTER_USERNAME"),
		os.Getenv("MESOS_EXPORTER_PASSWORD"),
	}

	var certPool *x509.CertPool = nil
	if *trustedCerts != "" {
		certPool = getX509CertPool(strings.Split(*trustedCerts, ","))
	}

	var tlsClientCert *tls.Certificate
	if *clientCert != "" && *clientKey != "" {
		cert, err := tls.LoadX509KeyPair(*clientCert, *clientKey)
		if err != nil {
			log.Fatalln("Failed to load client SSL certificate:", err)
		}
		tlsClientCert = &cert
		log.Println("Using SSL client certificates from:", *clientCert, *clientKey)
	}

	switch {
	case *masterURL != "":
		for _, f := range []func(*httpClient) prometheus.Collector{
			newMasterCollector,
			func(c *httpClient) prometheus.Collector {
				return newMasterStateCollector(c, *ignoreCompletedFrameworkTasks)
			},
		} {
			c := f(mkHttpClient(*masterURL, *timeout, auth, certPool, tlsClientCert));
			if err := prometheus.Register(c); err != nil {
				log.Fatal(err)
			}
		}
		log.Printf("Exposing master metrics on %s", *addr)

	case *slaveURL != "":
		slaveCollectors := []func(*httpClient) prometheus.Collector{
			func(c *httpClient) prometheus.Collector {
				return newSlaveCollector(c)
			},
			func(c *httpClient) prometheus.Collector {
				return newSlaveMonitorCollector(c)
			},
		}
		if *exportedTaskLabels != "" {
			slaveLabels := strings.Split(*exportedTaskLabels, ",");
			slaveCollectors = append(slaveCollectors, func (c *httpClient) prometheus.Collector{
				return newSlaveStateCollector(c, slaveLabels)
			})
		}

		for _, f := range slaveCollectors {
			c := f(mkHttpClient(*slaveURL, *timeout, auth, certPool, tlsClientCert));
			if err := prometheus.Register(c); err != nil {
				log.Fatal(err)
			}
		}
		log.Printf("Exposing slave metrics on %s", *addr)

	default:
		log.Fatal("Either -master or -slave is required")
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
            <head><title>Mesos Exporter</title></head>
            <body>
            <h1>Mesos Exporter</h1>
            <p><a href="/metrics">Metrics</a></p>
            </body>
            </html>`))
	})
	http.Handle("/metrics", prometheus.Handler())
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatal(err)
	}
}
