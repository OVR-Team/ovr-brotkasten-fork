package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"crypto/tls"
	"log"
	"net"
	"net/http"

	"broadcast-box/internal/networktest"
	"broadcast-box/internal/webrtc"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	envFileProd  = ".env.production"
	envFileDev   = ".env.development"
	envFileLocal = ".env.localhost"

	networkTestIntroMessage   = "\033[0;33mNETWORK_TEST_ON_START is enabled. If the test fails Broadcast Box will exit.\nSee the README for how to debug or disable NETWORK_TEST_ON_START\033[0m"
	networkTestSuccessMessage = "\033[0;32mNetwork Test passed.\nHave fun using Broadcast Box.\033[0m"
	networkTestFailedMessage  = "\033[0;31mNetwork Test failed.\n%s\nPlease see the README and join Discord for help\033[0m"
)

var errorFoo = errors.New("\033[0;31mBuild directory does not exist, run `npm install` and `npm run build` in the web directory.\033[0m")

type (
	whepLayerRequestJSON struct {
		MediaId    string `json:"mediaId"`
		EncodingId string `json:"encodingId"`
	}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// useful links:
// https://stackoverflow.com/questions/27410764/dial-with-a-specific-address-interface-golang
// https://stackoverflow.com/questions/22751035/golang-distinguish-ipv4-ipv6
func GetInterfaceIpv4Addr(interfaceName string) (addr string, err error) {
	var (
		ief      *net.Interface
		addrs    []net.Addr
		ipv4Addr net.IP
	)
	if ief, err = net.InterfaceByName(interfaceName); err != nil { // get interface
		return
	}
	if addrs, err = ief.Addrs(); err != nil { // get addresses
		return
	}
	for _, addr := range addrs { // get ipv4 address
		if ipv4Addr = addr.(*net.IPNet).IP.To4(); ipv4Addr != nil {
			break
		}
	}
	if ipv4Addr == nil {
		return "", fmt.Errorf("interface %s don't have an ipv4 address", interfaceName)
	}
	return ipv4Addr.String(), nil
}

func logHTTPError(w http.ResponseWriter, err string, code int) {
	log.Println(err)
	http.Error(w, err, code)
}

func validateStreamKey(streamKey string) bool {
	return regexp.MustCompile(`^[a-zA-Z0-9_\-\.~]+$`).MatchString(streamKey)
}

func extractBearerToken(authHeader string) (string, bool) {
	const bearerPrefix = "Bearer "
	if strings.HasPrefix(authHeader, bearerPrefix) {
		return strings.TrimPrefix(authHeader, bearerPrefix), true
	}
	return "", false
}

func whipConnectHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("whipConnectHandler(w: ", w, "r: ", r, ")")

	if r.Method == "DELETE" {
		return
	}

	log.Println("whipConnectHandler: before Upgrade")
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err.Error())
		return
	}
	log.Println("whipConnectHandler: after Upgrade")

	streamKeyHeader := r.Header.Get("Authorization")
	if streamKeyHeader == "" {
		logHTTPError(w, "Authorization was not set", http.StatusBadRequest)
		return
	}

	streamKey, ok := extractBearerToken(streamKeyHeader)
	if !ok || !validateStreamKey(streamKey) {
		logHTTPError(w, "Invalid stream key format", http.StatusBadRequest)
		return
	}

	webrtc.WHIPCreate(streamKey, conn)
}

func whepConnectHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("whepConnectHandler(w: ", w, "r: ", r, ")")
	log.Println("whepConnectHandler")

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err.Error())
		return
	}

	log.Println("whepConnectHandler: request", r)

	streamKeyHeader := r.Header.Get("Authorization")
	if streamKeyHeader == "" {
		logHTTPError(w, "Authorization was not set", http.StatusBadRequest)
		return
	}

	streamKey, ok := extractBearerToken(streamKeyHeader)
	if !ok || !validateStreamKey(streamKey) {
		logHTTPError(w, "Invalid stream key format", http.StatusBadRequest)
		return
	}

	log.Println("whepConnectHandler: before calling WHEPCreate", streamKey, conn)
	webrtc.WHEPCreate(streamKey, conn)
	log.Println("whepConnectHandler: after calling WHEPCreate")
}

func whepServerSentEventsHandler(res http.ResponseWriter, req *http.Request) {
	res.Header().Set("Content-Type", "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.Header().Set("Connection", "keep-alive")

	vals := strings.Split(req.URL.RequestURI(), "/")
	whepSessionId := vals[len(vals)-1]

	layers, err := webrtc.WHEPLayers(whepSessionId)
	if err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Fprint(res, "event: layers\n")
	fmt.Fprintf(res, "data: %s\n", string(layers))
	fmt.Fprint(res, "\n\n")
}

func whepLayerHandler(res http.ResponseWriter, req *http.Request) {
	var r whepLayerRequestJSON
	if err := json.NewDecoder(req.Body).Decode(&r); err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	vals := strings.Split(req.URL.RequestURI(), "/")
	whepSessionId := vals[len(vals)-1]

	if err := webrtc.WHEPChangeLayer(whepSessionId, r.EncodingId); err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}
}

func statusHandler(res http.ResponseWriter, req *http.Request) {
	res.Header().Add("Content-Type", "application/json")

	if err := json.NewEncoder(res).Encode(webrtc.GetStreamStatuses()); err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
	}
}

func indexHTMLWhenNotFound(fs http.FileSystem) http.Handler {
	fileServer := http.FileServer(fs)

	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		_, err := fs.Open(path.Clean(req.URL.Path)) // Do not allow path traversals.
		if errors.Is(err, os.ErrNotExist) {
			http.ServeFile(resp, req, "./web/build/index.html")

			return
		}
		fileServer.ServeHTTP(resp, req)
	})
}

func corsHandler(next func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		res.Header().Set("Access-Control-Allow-Origin", "*")
		res.Header().Set("Access-Control-Allow-Methods", "*")
		res.Header().Set("Access-Control-Allow-Headers", "*")
		res.Header().Set("Access-Control-Expose-Headers", "*")

		if req.Method != http.MethodOptions {
			next(res, req)
		}
	}
}

func main() {
	var ipv4Addr, err = GetInterfaceIpv4Addr("eth0")
	if err != nil {
		log.Println("ipv4 err: ", err)
	}

	loadConfigs := func() error {
		log.Println("Loading configs", os.Getenv("APP_ENV"))
		if os.Getenv("APP_ENV") == "development" {
			log.Println("Loading `" + envFileDev + "`")
			return godotenv.Load(envFileDev)
		} else if os.Getenv("APP_ENV") == "localhost" {
			log.Println("Loading `" + envFileLocal + "`")
			return godotenv.Load(envFileLocal)
		} else {
			log.Println("Loading `" + envFileProd + "`")
			if err := godotenv.Load(envFileProd); err != nil {
				return err
			}

			if _, err := os.Stat("./web/build"); os.IsNotExist(err) && os.Getenv("DISABLE_FRONTEND") == "" {
				return errorFoo
			}

			return nil
		}
	}

	if err := loadConfigs(); err != nil {
		log.Println("Failed to find config in CWD, changing CWD to executable path")

		exePath, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}

		if err = os.Chdir(filepath.Dir(exePath)); err != nil {
			log.Fatal(err)
		}

		if err = loadConfigs(); err != nil {
			log.Fatal(err)
		}
	}

	webrtc.Configure()

	if os.Getenv("NETWORK_TEST_ON_START") == "true" {
		fmt.Println(networkTestIntroMessage) //nolint

		go func() {
			time.Sleep(time.Second * 5)

			if networkTestErr := networktest.Run(whepConnectHandler); networkTestErr != nil {
				fmt.Printf(networkTestFailedMessage, networkTestErr.Error())
				os.Exit(1)
			} else {
				fmt.Println(networkTestSuccessMessage) //nolint
			}
		}()
	}

	httpsRedirectPort := "80"
	if val := os.Getenv("HTTPS_REDIRECT_PORT"); val != "" {
		httpsRedirectPort = val
	}

	if os.Getenv("HTTPS_REDIRECT_PORT") != "" || os.Getenv("ENABLE_HTTP_REDIRECT") != "" {
		go func() {
			redirectServer := &http.Server{
				Addr: ":" + httpsRedirectPort,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
				}),
			}

			log.Println("Running HTTP->HTTPS redirect Server at :" + httpsRedirectPort)
			log.Fatal(redirectServer.ListenAndServe())
		}()

	}

	mux := http.NewServeMux()
	if os.Getenv("DISABLE_FRONTEND") == "" {
		mux.Handle("/", indexHTMLWhenNotFound(http.Dir("./web/build")))
	}
	mux.HandleFunc("/api/whip/connect", whipConnectHandler)
	mux.HandleFunc("/api/whep/connect", whepConnectHandler)
	mux.HandleFunc("/api/sse/", corsHandler(whepServerSentEventsHandler))
	mux.HandleFunc("/api/layer/", corsHandler(whepLayerHandler))

	// Prometheus metrics, https://prometheus.io/docs/guides/go-application/

	http.Handle("/metrics", promhttp.Handler())

	if os.Getenv("DISABLE_STATUS") == "" {
		mux.HandleFunc("/api/status", corsHandler(statusHandler))
	}

	server := &http.Server{
		Handler: mux,
		Addr:    ipv4Addr + os.Getenv("HTTP_ADDRESS"),
	}

	tlsKey := os.Getenv("SSL_KEY")
	tlsCert := os.Getenv("SSL_CERT")

	if tlsKey != "" && tlsCert != "" {
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{},
		}

		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			log.Fatal(err)
		}

		server.TLSConfig.Certificates = append(server.TLSConfig.Certificates, cert)

		log.Println("Running HTTPS Server at `" + ipv4Addr + os.Getenv("HTTP_ADDRESS") + "`")
		log.Fatal(server.ListenAndServeTLS("", ""))
	} else {
		log.Println("Running HTTP Server at `" + ipv4Addr + os.Getenv("HTTP_ADDRESS") + "`")
		log.Fatal(server.ListenAndServe())
	}

}
