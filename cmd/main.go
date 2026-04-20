package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/qufeeh/edge-restreamer/config"
	"github.com/qufeeh/edge-restreamer/internal/metrics"
	"github.com/qufeeh/edge-restreamer/internal/muxer"
	"github.com/qufeeh/edge-restreamer/internal/parser"
	"github.com/qufeeh/edge-restreamer/internal/producer"
	"github.com/qufeeh/edge-restreamer/internal/srt"
	"github.com/qufeeh/edge-restreamer/internal/storage"
)

func main() {
	log.Println("Starting Edge-ReStreamer...")

	cfg, err := config.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	reader := producer.NewRTSPReader(cfg.Camera.URL)

	tsStream, err := reader.Start(ctx)
	if err != nil {
		log.Fatalf("Failed to start RTSP stream: %v", err)
	}

	gopParser := parser.NewGOPParser(tsStream)
	go gopParser.Start(ctx)

	hlsMuxer := muxer.NewHLSMuxer(
		cfg.Restream.HLS.TargetPath,
		int(cfg.Restream.HLS.SegmentDuration.Seconds()),
	)
	go hlsMuxer.Start(ctx)

	// инициализация MinIO
	uploader, err := storage.NewMinioUploader(
		cfg.Storage.S3Endpoint,
		cfg.Storage.AccessKey,
		cfg.Storage.SecretKey,
		cfg.Storage.Bucket,
	)
	if err != nil {
		log.Fatalf("Failed to init MinIO: %v", err)
	}

	var wg sync.WaitGroup

	// воркер s3
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Println("Storage worker started")

		for filename := range hlsMuxer.UploadChan {
			filePath := filepath.Join(cfg.Restream.HLS.TargetPath, filename)

			err := uploader.UploadFile(context.Background(), filePath, filename)
			if err != nil {
				log.Printf("MinIO Upload Error [%s]: %v", filename, err)
			} else {
				if !strings.HasSuffix(filename, ".m3u8") {
					log.Printf("S3: Uploaded %s", filename)
				}
			}
		}
		log.Println("Storage worker finished all pending uploads.")
	}()

	// инициализируем SRT Sender
	srtSender, err := srt.NewSender(
		cfg.Restream.SRT.Host,
		120,
	)
	if err != nil {
		log.Fatalf("Failed to init SRT: %v", err)
	}
	go srtSender.Start(ctx)

	statsCollector := metrics.NewCollector()
	go statsCollector.Start(ctx, 1*time.Second)

	go func() {
		framesCh := gopParser.Out()
		hlsIn := hlsMuxer.In()
		srtIn := srtSender.In()

		gopCount := 0

		for {
			select {
			case <-ctx.Done():
				close(hlsIn)
				close(srtIn)
				return
			case frame, ok := <-framesCh:
				if !ok {
					close(hlsIn)
					close(srtIn)
					return
				}

				statsCollector.AddFrame(len(frame.Data))
				if frame.IsGOPStart {
					gopCount++
					log.Printf("GOP: %d started! (IDR Frame, PTS: %d)", gopCount, frame.PTS)
				}

				// отдаем кадр в HLS муксер
				select {
				case hlsIn <- frame:
				default:
					statsCollector.AddDrop() // фиксируем дропы, если HLS или диск тупит
					log.Println("Warning: HLS muxer is too slow, dropping frame")
				}
				// отдаем копию кадра в SRT
				select {
				case srtIn <- frame:
				default:
					statsCollector.AddDrop()
					log.Println("Warning: SRT sender is too slow, dropping frame")
				}
			}
		}
	}()

	// HTTP Сервер
	go func() {
		log.Println("Starting local HTTP server, WebSocket and Prometheus exporter...")
		fs := http.FileServer(http.Dir("./www/hls"))
		mux := http.NewServeMux()

		// раздача статики HLS
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			fs.ServeHTTP(w, r)
		})

		mux.HandleFunc("/ws", statsCollector.ServeWS)

		mux.Handle("/metrics", promhttp.Handler())

		if err := http.ListenAndServe(":8080", mux); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down... waiting for subprocesses")

	if err := reader.Wait(); err != nil {
		log.Printf("RTSP reader finished with error: %v", err)
	}

	log.Println("Waiting for MinIO uploads to finish...")
	wg.Wait() // главный поток останавливается здесь и ждет defer wg.Done() из воркера
	log.Println("Graceful shutdown complete.")
}
