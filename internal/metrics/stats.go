package metrics

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/shirou/gopsutil/v3/cpu"
)

var (
	promFramesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "edge_restreamer_frames_total",
		Help: "Общее количество обработанных видеокадров",
	})
	promBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "edge_restreamer_bytes_total",
		Help: "Общее количество обработанных байт видео",
	})
	promDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "edge_restreamer_dropped_frames_total",
		Help: "Количество отброшенных кадров (из-за перегрузки)",
	})
	promCPUPercent = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "edge_restreamer_cpu_percent",
		Help: "Текущая загрузка CPU рестримером",
	})
)

type Stats struct {
	FPS         float64 `json:"fps"`
	BitrateMbps float64 `json:"bitrate_mbps"`
	CPUPercent  float64 `json:"cpu_percent"`
	Dropped     uint64  `json:"dropped_frames"`
}

type Collector struct {
	frames  uint64
	bytes   uint64
	dropped uint64

	clients  map[*websocket.Conn]bool
	mu       sync.Mutex
	upgrader websocket.Upgrader
}

func NewCollector() *Collector {
	return &Collector{
		clients: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// фиксирует приход нового кадра
func (c *Collector) AddFrame(sizeBytes int) {
	// Для WebSocket
	atomic.AddUint64(&c.frames, 1)
	atomic.AddUint64(&c.bytes, uint64(sizeBytes))

	// Для Prometheus
	promFramesTotal.Inc()
	promBytesTotal.Add(float64(sizeBytes))
}

// фиксирует кадры, которые не пролезли в муксер
func (c *Collector) AddDrop() {
	// Для WebSocket
	atomic.AddUint64(&c.dropped, 1)

	// Для Prometheus
	promDroppedTotal.Inc()
}

func (c *Collector) Start(ctx context.Context, interval time.Duration) {
	log.Println("Metrics Collector started")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// снимаем показания и обнуляем временные счетчики для WS
			f := atomic.SwapUint64(&c.frames, 0)
			b := atomic.SwapUint64(&c.bytes, 0)
			d := atomic.LoadUint64(&c.dropped) // ошибки только читаем

			// загрузка CPU
			cpuPercents, _ := cpu.Percent(0, false)
			cpuVal := 0.0
			if len(cpuPercents) > 0 {
				cpuVal = cpuPercents[0]
			}

			// обновляем Gauge в Prometheus
			promCPUPercent.Set(cpuVal)

			stats := Stats{
				FPS:         float64(f) / interval.Seconds(),
				BitrateMbps: (float64(b) * 8) / 1000000.0 / interval.Seconds(),
				CPUPercent:  cpuVal,
				Dropped:     d,
			}

			c.broadcast(stats)
		}
	}
}

func (c *Collector) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := c.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS Upgrade error: %v", err)
		return
	}

	c.mu.Lock()
	c.clients[conn] = true
	c.mu.Unlock()

	log.Printf("Dashboard connected: %s", conn.RemoteAddr())
}

func (c *Collector) broadcast(stats Stats) {
	msg, err := json.Marshal(stats)
	if err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for conn := range c.clients {
		err := conn.WriteMessage(websocket.TextMessage, msg)
		if err != nil {
			conn.Close()
			delete(c.clients, conn)
		}
	}
}
