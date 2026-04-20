package srt

import (
	"bufio"
	"context"
	"log"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	srt "github.com/datarhei/gosrt"
	"github.com/qufeeh/edge-restreamer/internal/parser" // НЕ ЗАБУДЬ ПОМЕНЯТЬ ПУТЬ
)

type Sender struct {
	address string
	latency int
	in      chan parser.VideoFrame

	conn     srt.Conn // Используем чистый Go SRT сокет
	tsWriter *mpegts.Writer
	track    *mpegts.Track
	buffer   *bufio.Writer

	sps []byte
	pps []byte
}

func NewSender(address string, latencyMs int) (*Sender, error) {
	return &Sender{
		address: address, // Формат "host:port", например "127.0.0.1:9000"
		latency: latencyMs,
		in:      make(chan parser.VideoFrame, 100),
	}, nil
}

func (s *Sender) In() chan<- parser.VideoFrame {
	return s.in
}

func (s *Sender) Start(ctx context.Context) {
	log.Printf("SRT Sender connecting to %s (latency: %dms)...", s.address, s.latency)

	// Настраиваем SRT (чистый Go!)
	conf := srt.DefaultConfig()
	// Можно передать latency в конфиг, если потребуется тонкая настройка

	// Подключаемся к приемнику
	conn, err := srt.Dial("srt", s.address, conf)
	if err != nil {
		log.Printf("SRT Connection failed: %v", err)
		return
	}
	defer conn.Close()
	s.conn = conn

	log.Println("SRT Sender connected successfully!")

	// TS-пакеты обычно передаются чанками по 1316 байт
	s.buffer = bufio.NewWriterSize(s.conn, 1316)

	for {
		select {
		case <-ctx.Done():
			if s.buffer != nil {
				s.buffer.Flush()
			}
			return
		case frame, ok := <-s.in:
			if !ok {
				return
			}
			s.processFrame(frame)
		}
	}
}

func (s *Sender) processFrame(frame parser.VideoFrame) {
	// 1. Инициализация TS Writer'а при первом ключевом кадре
	if s.tsWriter == nil {
		if len(frame.SPS) > 0 && len(frame.PPS) > 0 {
			s.sps = frame.SPS
			s.pps = frame.PPS

			s.track = &mpegts.Track{
				Codec: &tscodecs.H264{},
			}
			s.tsWriter = mpegts.NewWriter(s.buffer, []*mpegts.Track{s.track})
			log.Println("SRT: TS Writer initialized")
		} else {
			return // Ждем первый IDR с настройками
		}
	}

	// 2. Распаковываем NALU
	var annexb h264.AnnexB
	if err := annexb.Unmarshal(frame.Data); err != nil {
		return
	}

	// Для MPEG-TS мы НЕ вырезаем AUD, SPS и PPS!
	if len(annexb) == 0 {
		return
	}

	// 3. Упаковываем H.264 в MPEG-TS и пишем в буфер
	err := s.tsWriter.WriteH264(s.track, frame.PTS, frame.DTS, annexb)
	if err != nil {
		log.Printf("SRT write error: %v", err)
		return
	}

	// Отправляем данные в сокет
	s.buffer.Flush()
}
