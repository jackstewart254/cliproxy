package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sysstats"
)

// GetSystem reports the load on the host this proxy runs on.
func (h *Handler) GetSystem(c *gin.Context) {
	stats, err := sysstats.Read(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stats)
}

// deviceTimeout bounds one device's stats request, so a machine that is
// asleep or unplugged shows as offline instead of holding the dashboard up.
// Like the APICall timeout, it is a management call, not an upstream stream.
const deviceTimeout = 3 * time.Second

// deviceClient does not follow redirects: following one to another host
// would hand it the device's management key.
var deviceClient = &http.Client{
	Timeout:       deviceTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// deviceReply is one machine on the dashboard's Devices tab.
type deviceReply struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Local  bool            `json:"local"`
	Online bool            `json:"online"`
	Error  string          `json:"error,omitempty"`
	Stats  *sysstats.Stats `json:"stats,omitempty"`
}

// GetDashboardDevices returns this host's reading and each configured
// device's. The server fetches a device's reading itself, so the device's
// management key never reaches the browser.
func (h *Handler) GetDashboardDevices(c *gin.Context) {
	ctx := c.Request.Context()
	var devices []config.Device
	if h.cfg != nil {
		devices = h.cfg.RemoteManagement.Devices
	}
	out := make([]deviceReply, len(devices)+1)
	var wg sync.WaitGroup
	for i, d := range devices {
		wg.Go(func() { out[i+1] = h.fetchDevice(ctx, strconv.Itoa(i+1), d) })
	}
	out[0] = deviceReply{ID: "local", Local: true}
	if stats, err := sysstats.Read(ctx); err != nil {
		out[0].Error = err.Error()
	} else {
		out[0].Name, out[0].Online, out[0].Stats = stats.Hostname, true, stats
	}
	wg.Wait()
	c.JSON(http.StatusOK, gin.H{"devices": out})
}

// fetchDevice reads one device. Ids are positions in the config, so two
// devices with one name, or one named "local", stay apart.
func (h *Handler) fetchDevice(ctx context.Context, id string, d config.Device) deviceReply {
	reply := deviceReply{ID: id, Name: d.Name}
	pair := d.URL + "\x00" + d.ManagementKey
	if _, refused := h.refusedDeviceKeys.Load(pair); refused {
		reply.Error = "it refused this proxy's management key; fix management-key for it in this proxy's config to try again"
		return reply
	}
	stats, status, err := deviceStats(ctx, d)
	// Only a wrong key (401) counts toward the device's ban, so only that
	// stops the asking. A 403 (banned, management off) is not counted there
	// and is asked again on the next poll.
	if status == http.StatusUnauthorized {
		h.refusedDeviceKeys.Store(pair, struct{}{})
	}
	if err != nil {
		reply.Error = err.Error()
		return reply
	}
	reply.Online, reply.Stats = true, stats
	return reply
}

func deviceStats(ctx context.Context, d config.Device) (*sysstats.Stats, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.URL, "/")+"/v8/management/server/system", nil)
	if err != nil {
		return nil, 0, fmt.Errorf("its url is not valid: %w", err)
	}
	req.Header.Set("X-Management-Key", d.ManagementKey)
	resp, err := deviceClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			return nil, 0, fmt.Errorf("no answer within %s", deviceTimeout)
		}
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, 0, fmt.Errorf("unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, 1<<20)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, resp.StatusCode, errors.New("its proxy has no stats endpoint; it needs this build and a management secret-key")
	default:
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(body).Decode(&refusal)
		if refusal.Error != "" {
			return nil, resp.StatusCode, fmt.Errorf("it answered %s: %s", resp.Status, refusal.Error)
		}
		return nil, resp.StatusCode, fmt.Errorf("it answered %s", resp.Status)
	}
	var stats sysstats.Stats
	if err = json.NewDecoder(body).Decode(&stats); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("its reply could not be read: %w", err)
	}
	return &stats, resp.StatusCode, nil
}
