package clashapi

import (
	"bufio"
	"context"
	_ "embed"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/trafficcontrol"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing/common/json"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"github.com/oschwald/maxminddb-golang"
)

//go:embed trafficmap_ui.html
var trafficMapHTML []byte

type GeoLocation struct {
	IP          string  `json:"ip"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	City        string  `json:"city"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	ISP         string  `json:"isp"`
	Org         string  `json:"org"`
	AS          string  `json:"as"`
	IsPrivate   bool    `json:"isPrivate,omitempty"`
	Pending     bool    `json:"pending,omitempty"`
}

type MapConnectionItem struct {
	ID          string       `json:"id"`
	Network     string       `json:"network"`
	Inbound     string       `json:"inbound"`
	Host        string       `json:"host"`
	SourceIP    string       `json:"sourceIP"`
	SourcePort  uint16       `json:"sourcePort"`
	SourceGeo   *GeoLocation `json:"sourceGeo,omitempty"`
	OutboundTag string       `json:"outboundTag"`
	OutboundTyp string       `json:"outboundType"`
	OutboundIP  string       `json:"outboundIP,omitempty"`
	OutboundGeo *GeoLocation `json:"outboundGeo,omitempty"`
	DestIP      string       `json:"destIP"`
	DestPort    uint16       `json:"destPort"`
	DestGeo     *GeoLocation `json:"destGeo,omitempty"`
	Chains      []string     `json:"chains"`
	Rule        string       `json:"rule"`
	Upload      int64        `json:"upload"`
	Download    int64        `json:"download"`
	Start       time.Time    `json:"start"`
	ClosedAt    *time.Time   `json:"closedAt,omitempty"`
	Active      bool         `json:"active"`
}

type TraceHop struct {
	Hop     int          `json:"hop"`
	IP      string       `json:"ip"`
	RTT     string       `json:"rtt,omitempty"`
	Geo     *GeoLocation `json:"geo,omitempty"`
	Timeout bool         `json:"timeout,omitempty"`
}

type trafficMapService struct {
	server      *Server
	geoCache    sync.Map // string(ip) -> *GeoLocation
	geoInflight sync.Map // string(ip) -> bool
	geoQueue    chan string
	domainCache sync.Map // string(domain) -> netip.Addr
	domInflight sync.Map // string(domain) -> bool
	domQueue    chan domainResolveJob

	serverGeoMu   sync.RWMutex
	serverGeo     *GeoLocation
	serverGeoOnce sync.Once

	mmdbReader *maxminddb.Reader
	httpClient *http.Client
}

type domainResolveJob struct {
	domain  string
	tracker *trafficcontrol.TrackerMetadata
}

type mmdbCityRecord struct {
	Country struct {
		ISOCode string            `maxminddb:"iso_code"`
		Names   map[string]string `maxminddb:"names"`
	} `maxminddb:"country"`
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Location struct {
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

func newTrafficMapService(s *Server) *trafficMapService {
	svc := &trafficMapService{
		server:   s,
		geoQueue: make(chan string, 512),
		domQueue: make(chan domainResolveJob, 256),
		httpClient: &http.Client{
			Timeout: 4 * time.Second,
		},
	}
	svc.tryOpenMMDB()
	for range 4 {
		go svc.geoWorker()
	}
	for range 2 {
		go svc.domainWorker()
	}
	return svc
}

func (m *trafficMapService) tryOpenMMDB() {
	candidates := []string{
		os.Getenv("SING_BOX_GEOIP_CITY"),
		"GeoLite2-City.mmdb",
		"dbip-city-lite.mmdb",
		"/usr/share/GeoIP/GeoLite2-City.mmdb",
		"/var/lib/sing-box/GeoLite2-City.mmdb",
	}
	for _, path := range candidates {
		if path == "" {
			continue
		}
		reader, err := maxminddb.Open(path)
		if err == nil {
			m.mmdbReader = reader
			return
		}
	}
}

func isPrivateOrReservedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap()
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified()
}

func (m *trafficMapService) getGeoNonBlocking(ipStr string) *GeoLocation {
	if ipStr == "" {
		return nil
	}
	addr, err := netip.ParseAddr(ipStr)
	if err != nil || !addr.IsValid() {
		return nil
	}
	addr = addr.Unmap()
	ipKey := addr.String()
	if isPrivateOrReservedAddr(addr) {
		return &GeoLocation{
			IP:        ipKey,
			Country:   "Local Network",
			City:      "LAN / Loopback",
			IsPrivate: true,
		}
	}
	if cached, ok := m.geoCache.Load(ipKey); ok {
		return cached.(*GeoLocation)
	}
	if m.mmdbReader != nil {
		if loc := m.lookupMMDB(addr); loc != nil {
			m.geoCache.Store(ipKey, loc)
			return loc
		}
	}
	if _, loaded := m.geoInflight.LoadOrStore(ipKey, true); !loaded {
		select {
		case m.geoQueue <- ipKey:
		default:
			m.geoInflight.Delete(ipKey)
		}
	}
	return &GeoLocation{
		IP:      ipKey,
		Pending: true,
	}
}

func (m *trafficMapService) lookupGeoBlocking(ctx context.Context, ipStr string) *GeoLocation {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil || !addr.IsValid() {
		return nil
	}
	addr = addr.Unmap()
	ipKey := addr.String()
	if isPrivateOrReservedAddr(addr) {
		return &GeoLocation{
			IP:        ipKey,
			Country:   "Local Network",
			City:      "LAN / Loopback",
			IsPrivate: true,
		}
	}
	if cached, ok := m.geoCache.Load(ipKey); ok {
		return cached.(*GeoLocation)
	}
	if m.mmdbReader != nil {
		if loc := m.lookupMMDB(addr); loc != nil {
			m.geoCache.Store(ipKey, loc)
			return loc
		}
	}
	loc := m.fetchOnlineGeo(ctx, ipKey)
	if loc != nil {
		m.geoCache.Store(ipKey, loc)
	}
	return loc
}

func (m *trafficMapService) lookupMMDB(addr netip.Addr) *GeoLocation {
	var record mmdbCityRecord
	err := m.mmdbReader.Lookup(net.IP(addr.AsSlice()), &record)
	if err != nil || (record.Location.Latitude == 0 && record.Location.Longitude == 0) {
		return nil
	}
	country := record.Country.Names["zh-CN"]
	if country == "" {
		country = record.Country.Names["en"]
	}
	city := record.City.Names["zh-CN"]
	if city == "" {
		city = record.City.Names["en"]
	}
	return &GeoLocation{
		IP:          addr.String(),
		Country:     country,
		CountryCode: record.Country.ISOCode,
		City:        city,
		Lat:         record.Location.Latitude,
		Lon:         record.Location.Longitude,
	}
}

func (m *trafficMapService) geoWorker() {
	for ipKey := range m.geoQueue {
		loc := m.fetchOnlineGeo(m.server.ctx, ipKey)
		if loc != nil {
			m.geoCache.Store(ipKey, loc)
		}
		m.geoInflight.Delete(ipKey)
		// Gentle rate limit for batch bursts
		time.Sleep(120 * time.Millisecond)
	}
}

func (m *trafficMapService) fetchOnlineGeo(ctx context.Context, ipKey string) *GeoLocation {
	// Primary: ipwho.is (HTTPS, no strict 45 req/min limit, supports ISP/ASN)
	reqCtx, cancel := context.WithTimeout(ctx, 3500*time.Millisecond)
	defer cancel()

	url := "https://ipwho.is/" + ipKey
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err == nil {
		resp, err := m.httpClient.Do(req)
		if err == nil {
			var data struct {
				Success     bool    `json:"success"`
				IP          string  `json:"ip"`
				Country     string  `json:"country"`
				CountryCode string  `json:"country_code"`
				Region      string  `json:"region"`
				City        string  `json:"city"`
				Latitude    float64 `json:"latitude"`
				Longitude   float64 `json:"longitude"`
				Connection  struct {
					ASN int    `json:"asn"`
					Org string `json:"org"`
					ISP string `json:"isp"`
				} `json:"connection"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()
			if decodeErr == nil && data.Success && (data.Latitude != 0 || data.Longitude != 0) {
				asStr := ""
				if data.Connection.ASN > 0 {
					asStr = "AS" + strconv.Itoa(data.Connection.ASN)
				}
				city := data.City
				if city == "" {
					city = data.Region
				}
				return &GeoLocation{
					IP:          ipKey,
					Country:     data.Country,
					CountryCode: data.CountryCode,
					City:        city,
					Lat:         data.Latitude,
					Lon:         data.Longitude,
					ISP:         data.Connection.ISP,
					Org:         data.Connection.Org,
					AS:          asStr,
				}
			}
		}
	}

	// Fallback: ip-api.com
	url2 := "http://ip-api.com/json/" + ipKey + "?fields=status,country,countryCode,regionName,city,lat,lon,isp,org,as,query"
	req2, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url2, nil)
	if err == nil {
		resp2, err := m.httpClient.Do(req2)
		if err == nil {
			var data2 struct {
				Status      string  `json:"status"`
				Country     string  `json:"country"`
				CountryCode string  `json:"countryCode"`
				RegionName  string  `json:"regionName"`
				City        string  `json:"city"`
				Lat         float64 `json:"lat"`
				Lon         float64 `json:"lon"`
				ISP         string  `json:"isp"`
				Org         string  `json:"org"`
				AS          string  `json:"as"`
			}
			decodeErr := json.NewDecoder(resp2.Body).Decode(&data2)
			resp2.Body.Close()
			if decodeErr == nil && data2.Status == "success" {
				city := data2.City
				if city == "" {
					city = data2.RegionName
				}
				return &GeoLocation{
					IP:          ipKey,
					Country:     data2.Country,
					CountryCode: data2.CountryCode,
					City:        city,
					Lat:         data2.Lat,
					Lon:         data2.Lon,
					ISP:         data2.ISP,
					Org:         data2.Org,
					AS:          data2.AS,
				}
			}
		}
	}
	return nil
}

func (m *trafficMapService) ensureServerGeo() *GeoLocation {
	m.serverGeoMu.RLock()
	current := m.serverGeo
	m.serverGeoMu.RUnlock()
	if current != nil {
		return current
	}
	go m.serverGeoOnce.Do(func() {
		ctx, cancel := context.WithTimeout(m.server.ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ipwho.is/", nil)
		if err == nil {
			resp, err := m.httpClient.Do(req)
			if err == nil {
				var data struct {
					Success     bool    `json:"success"`
					IP          string  `json:"ip"`
					Country     string  `json:"country"`
					CountryCode string  `json:"country_code"`
					City        string  `json:"city"`
					Latitude    float64 `json:"latitude"`
					Longitude   float64 `json:"longitude"`
					Connection  struct {
						ASN int    `json:"asn"`
						Org string `json:"org"`
						ISP string `json:"isp"`
					} `json:"connection"`
				}
				decodeErr := json.NewDecoder(resp.Body).Decode(&data)
				resp.Body.Close()
				if decodeErr == nil && data.Success {
					loc := &GeoLocation{
						IP:          data.IP,
						Country:     data.Country,
						CountryCode: data.CountryCode,
						City:        data.City,
						Lat:         data.Latitude,
						Lon:         data.Longitude,
						ISP:         data.Connection.ISP,
						Org:         data.Connection.Org,
					}
					m.serverGeoMu.Lock()
					m.serverGeo = loc
					m.serverGeoMu.Unlock()
					m.geoCache.Store(data.IP, loc)
					return
				}
			}
		}
		// Fallback to ip-api.com
		req2, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ip-api.com/json/?fields=status,country,countryCode,city,lat,lon,isp,org,as,query", nil)
		if err == nil {
			resp2, err := m.httpClient.Do(req2)
			if err == nil {
				var data2 struct {
					Status      string  `json:"status"`
					Query       string  `json:"query"`
					Country     string  `json:"country"`
					CountryCode string  `json:"countryCode"`
					City        string  `json:"city"`
					Lat         float64 `json:"lat"`
					Lon         float64 `json:"lon"`
					ISP         string  `json:"isp"`
					Org         string  `json:"org"`
					AS          string  `json:"as"`
				}
				decodeErr := json.NewDecoder(resp2.Body).Decode(&data2)
				resp2.Body.Close()
				if decodeErr == nil && data2.Status == "success" {
					loc := &GeoLocation{
						IP:          data2.Query,
						Country:     data2.Country,
						CountryCode: data2.CountryCode,
						City:        data2.City,
						Lat:         data2.Lat,
						Lon:         data2.Lon,
						ISP:         data2.ISP,
						Org:         data2.Org,
						AS:          data2.AS,
					}
					m.serverGeoMu.Lock()
					m.serverGeo = loc
					m.serverGeoMu.Unlock()
					m.geoCache.Store(data2.Query, loc)
				}
			}
		}
	})
	return nil
}

func (m *trafficMapService) domainWorker() {
	for job := range m.domQueue {
		ctx, cancel := context.WithTimeout(m.server.ctx, 3*time.Second)
		var resolved netip.Addr
		if m.server.dnsRouter != nil {
			addrs, err := m.server.dnsRouter.Lookup(ctx, job.domain, adapter.DNSQueryOptions{})
			if err == nil && len(addrs) > 0 {
				resolved = addrs[0].Unmap()
			}
		}
		if !resolved.IsValid() {
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", job.domain)
			if err == nil && len(ips) > 0 {
				resolved = ips[0].Unmap()
			}
		}
		cancel()
		if resolved.IsValid() {
			m.domainCache.Store(job.domain, resolved)
			if job.tracker != nil {
				job.tracker.RecordResolvedAddr(resolved)
			}
			// Pre-warm GeoIP
			m.getGeoNonBlocking(resolved.String())
		}
		m.domInflight.Delete(job.domain)
	}
}

func (m *trafficMapService) resolveDomainNonBlocking(domain string, tm *trafficcontrol.TrackerMetadata) netip.Addr {
	if domain == "" {
		return netip.Addr{}
	}
	if cached, ok := m.domainCache.Load(domain); ok {
		addr := cached.(netip.Addr)
		if tm != nil {
			tm.RecordResolvedAddr(addr)
		}
		return addr
	}
	if _, loaded := m.domInflight.LoadOrStore(domain, true); !loaded {
		select {
		case m.domQueue <- domainResolveJob{domain: domain, tracker: tm}:
		default:
			m.domInflight.Delete(domain)
		}
	}
	return netip.Addr{}
}

func (m *trafficMapService) buildConnectionItem(tm *trafficcontrol.TrackerMetadata, active bool) MapConnectionItem {
	var inbound string
	if tm.Metadata.Inbound != "" {
		inbound = tm.Metadata.InboundType + "/" + tm.Metadata.Inbound
	} else {
		inbound = tm.Metadata.InboundType
	}
	domain := tm.Metadata.Domain
	if domain == "" {
		domain = tm.Metadata.Destination.Fqdn
	}
	var rule string
	if tm.Rule != nil {
		rule = tm.Rule.String() + " => " + tm.Rule.Action().String()
	} else {
		rule = "final"
	}

	sourceIP := ""
	if tm.Metadata.Source.Addr.IsValid() {
		sourceIP = tm.Metadata.Source.Addr.Unmap().String()
	}
	destAddr := tm.DestinationAddr()
	if !destAddr.IsValid() && domain != "" {
		destAddr = m.resolveDomainNonBlocking(domain, tm)
	}
	destIP := ""
	if destAddr.IsValid() {
		destIP = destAddr.String()
	}

	outboundAddr := tm.OutboundAddr()
	outboundIP := ""
	if outboundAddr.IsValid() {
		outboundIP = outboundAddr.String()
	}

	var closedAtPtr *time.Time
	if !active && !tm.ClosedAt.IsZero() {
		t := tm.ClosedAt
		closedAtPtr = &t
	}

	return MapConnectionItem{
		ID:          tm.ID.String(),
		Network:     tm.Metadata.Network,
		Inbound:     inbound,
		Host:        domain,
		SourceIP:    sourceIP,
		SourcePort:  tm.Metadata.Source.Port,
		SourceGeo:   m.getGeoNonBlocking(sourceIP),
		OutboundTag: tm.Outbound,
		OutboundTyp: tm.OutboundType,
		OutboundIP:  outboundIP,
		OutboundGeo: m.getGeoNonBlocking(outboundIP),
		DestIP:      destIP,
		DestPort:    tm.Metadata.Destination.Port,
		DestGeo:     m.getGeoNonBlocking(destIP),
		Chains:      tm.Chain,
		Rule:        rule,
		Upload:      tm.Upload.Load(),
		Download:    tm.Download.Load(),
		Start:       tm.CreatedAt,
		ClosedAt:    closedAtPtr,
		Active:      active,
	}
}

func (m *trafficMapService) handleState(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	recentWindow := 45 * time.Second
	if windowStr := r.URL.Query().Get("window"); windowStr != "" {
		if sec, err := strconv.Atoi(windowStr); err == nil && sec >= 0 && sec <= 600 {
			recentWindow = time.Duration(sec) * time.Second
		}
	}

	activeConns := m.server.trafficManager.Connections()
	closedConns := m.server.trafficManager.ClosedConnections()

	items := make([]MapConnectionItem, 0, len(activeConns)+32)
	for _, tm := range activeConns {
		if tm.OutboundType == C.TypeDNS {
			continue
		}
		items = append(items, m.buildConnectionItem(tm, true))
	}

	// Include recently closed connections (newest first, up to 80 items) so fast HTTP visits still appear smoothly
	recentCount := 0
	for i := len(closedConns) - 1; i >= 0 && recentCount < 80; i-- {
		tm := closedConns[i]
		if tm.OutboundType == C.TypeDNS {
			continue
		}
		if now.Sub(tm.ClosedAt) > recentWindow {
			continue
		}
		items = append(items, m.buildConnectionItem(tm, false))
		recentCount++
	}

	upTotal, downTotal := m.server.trafficManager.Total()
	render.JSON(w, r, render.M{
		"server":        m.ensureServerGeo(),
		"uploadTotal":   upTotal,
		"downloadTotal": downTotal,
		"connections":   items,
		"timestamp":     now,
	})
}

var ipv4or6Regex = regexp.MustCompile(`([0-9]{1,3}(?:\.[0-9]{1,3}){3}|[0-9a-fA-F:]{3,})`)
var rttRegex = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?\s*ms)`)

func (m *trafficMapService) handleTrace(w http.ResponseWriter, r *http.Request) {
	targetIP := strings.TrimSpace(r.URL.Query().Get("ip"))
	addr, err := netip.ParseAddr(targetIP)
	if err != nil || !addr.IsValid() {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, render.M{"error": "invalid target IP"})
		return
	}
	targetIP = addr.Unmap().String()

	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()

	hops := m.runSystemTraceroute(ctx, targetIP)
	render.JSON(w, r, render.M{
		"targetIP":  targetIP,
		"targetGeo": m.lookupGeoBlocking(ctx, targetIP),
		"serverGeo": m.ensureServerGeo(),
		"hops":      hops,
	})
}

func (m *trafficMapService) runSystemTraceroute(ctx context.Context, targetIP string) []TraceHop {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "tracert", "-d", "-h", "18", "-w", "400", targetIP)
	} else if _, err := exec.LookPath("traceroute"); err == nil {
		cmd = exec.CommandContext(ctx, "traceroute", "-n", "-q", "1", "-w", "1", "-m", "18", targetIP)
	} else if _, err := exec.LookPath("tracepath"); err == nil {
		cmd = exec.CommandContext(ctx, "tracepath", "-n", "-m", "18", targetIP)
	}

	var hops []TraceHop
	if cmd != nil {
		out, _ := cmd.CombinedOutput()
		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		hopIndex := 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			hopNumStr := strings.TrimRight(fields[0], ":")
			hopNum, err := strconv.Atoi(hopNumStr)
			if err != nil || hopNum <= 0 || hopNum > 64 {
				continue
			}
			hopIndex = hopNum
			rest := strings.Join(fields[1:], " ")
			ipMatches := ipv4or6Regex.FindAllString(rest, -1)
			var foundIP string
			for _, candidate := range ipMatches {
				if parsed, parseErr := netip.ParseAddr(candidate); parseErr == nil && parsed.IsValid() {
					foundIP = parsed.Unmap().String()
					break
				}
			}
			if foundIP == "" {
				hops = append(hops, TraceHop{
					Hop:     hopIndex,
					Timeout: true,
				})
				continue
			}
			rtt := rttRegex.FindString(rest)
			hops = append(hops, TraceHop{
				Hop: hopIndex,
				IP:  foundIP,
				RTT: rtt,
				Geo: m.lookupGeoBlocking(ctx, foundIP),
			})
			if foundIP == targetIP {
				break
			}
		}
	}

	// Ensure final target IP is always represented as the last hop if traceroute binary wasn't installed or didn't reach
	if len(hops) == 0 || hops[len(hops)-1].IP != targetIP {
		hops = append(hops, TraceHop{
			Hop: len(hops) + 1,
			IP:  targetIP,
			Geo: m.lookupGeoBlocking(ctx, targetIP),
		})
	}
	return hops
}

func (s *Server) setupTrafficMap(chiRouter chi.Router, secret string) {
	mapService := newTrafficMapService(s)

	// Public HTML entrypoint (authenticates API calls using token from query/localStorage)
	chiRouter.Get("/map", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(trafficMapHTML)
	})

	chiRouter.Group(func(r chi.Router) {
		r.Use(authentication(secret))
		r.Get("/map/api/state", mapService.handleState)
		r.Get("/map/api/trace", mapService.handleTrace)
	})
}
