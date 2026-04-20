package muxer

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/qufeeh/edge-restreamer/internal/parser"
)

type segment struct {
	Name     string
	Duration float64
}

type llPart struct {
	Name     string
	Duration float64
}

type HLSMuxer struct {
	targetPath      string
	segmentDuration int
	partDurationSec float64 // Длительность ll-hls части ~0.5s
	in              chan parser.VideoFrame

	segmentIndex int
	partIndex    int // индекс текущей части внутри сегмента
	sps          []byte
	pps          []byte
	segments     []segment
	currentParts []llPart // части, которые сейчас пишутся (до схлопывания в сегмент)

	segmentSamples []*fmp4.PartSample // все кадры для финального seg-X.m4s
	partSamples    []*fmp4.PartSample // кадры только для текущего seg-X-part-Y.m4s

	segBaseTime     uint64 // глобальное время начала текущего сегмента
	partBaseTime    uint64 // глобальное время начала текущей микро-части
	lastPTS         uint64
	currentSegTime  uint64
	currentPartTime uint64
	globalSeqNum    uint32 // единый sequence для всех fmp4 контейнеров

	UploadChan chan string
}

func NewHLSMuxer(targetPath string, segDuration int) *HLSMuxer {
	os.MkdirAll(targetPath, 0755)
	return &HLSMuxer{
		targetPath:      targetPath,
		segmentDuration: segDuration,
		partDurationSec: 0.5, // каждые полсекунды выдаем ll-hls чанк
		in:              make(chan parser.VideoFrame, 100),
		segments:        make([]segment, 0),
		currentParts:    make([]llPart, 0),
		segmentSamples:  make([]*fmp4.PartSample, 0),
		partSamples:     make([]*fmp4.PartSample, 0),
		segBaseTime:     0,
		partBaseTime:    0,
		globalSeqNum:    1,
		UploadChan:      make(chan string, 50),
	}
}

func (m *HLSMuxer) In() chan<- parser.VideoFrame {
	return m.in
}

func (m *HLSMuxer) Start(ctx context.Context) {
	log.Println("LL-HLS Muxer started")
	defer log.Println("LL-HLS Muxer stopped")

	// когда муксер завершит работу - он закроет канал.
	// этот сигнал для воркера minio, что навых файлов больше не будет
	defer close(m.UploadChan)

	for {
		select {
		case <-ctx.Done():
			// спасаем кадры, которые зависли в буфере перед выкл.
			if len(m.partSamples) > 0 {
				m.flushPart()
			}
			if len(m.segmentSamples) > 0 {
				m.flushSegment()
			}
			return
		case frame, ok := <-m.in:
			if !ok {
				return
			}
			m.writeFrame(frame)
		}
	}
}

func extractNALUs(data []byte) [][]byte {
	var nalus [][]byte
	start := 0
	for i := 0; i < len(data)-2; i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				if i > start {
					nalus = append(nalus, data[start:i])
				}
				start = i + 3
				i += 2
			} else if i < len(data)-3 && data[i+2] == 0 && data[i+3] == 1 {
				if i > start {
					nalus = append(nalus, data[start:i])
				}
				start = i + 4
				i += 3
			}
		}
	}
	if start < len(data) {
		nalus = append(nalus, data[start:])
	}
	return nalus
}

func (m *HLSMuxer) writeFrame(frame parser.VideoFrame) {
	if m.sps == nil {
		if len(frame.SPS) > 0 && len(frame.PPS) > 0 {
			m.sps = frame.SPS
			m.pps = frame.PPS
			m.writeInitSegment()
		} else {
			return
		}
	}

	// извлекаем сырые байты без сторонних библиотек
	rawNALUs := extractNALUs(frame.Data)

	// оставляем ТОЛЬКО чистые пиксели, убиваем весь мусор
	var cleanNALUs [][]byte
	for _, nalu := range rawNALUs {
		if len(nalu) == 0 {
			continue
		}
		typ := nalu[0] & 0x1F
		if typ == 1 || typ == 5 {
			cleanNALUs = append(cleanNALUs, nalu)
		}
	}

	if len(cleanNALUs) == 0 {
		return
	}

	// AVCC формат переварит чистые данные без проблем
	avccPayload, err := h264.AVCC(cleanNALUs).Marshal()
	if err != nil {
		return
	}

	// считаем реальную длительность кадра на основе таймингов камеры
	pts := uint64(frame.PTS)
	duration := uint32(3600)
	if m.lastPTS != 0 && pts > m.lastPTS {
		duration = uint32(pts - m.lastPTS)
	}
	m.lastPTS = pts
	isIDR := frame.IsGOPStart

	sample := &fmp4.PartSample{
		Duration:        duration,
		IsNonSyncSample: !isIDR,
		Payload:         avccPayload,
	}

	// нарезка (с допуском 0.1)
	shouldCutSeg := false
	if isIDR && len(m.segmentSamples) > 0 {
		accumulatedSec := float64(m.currentSegTime) / 90000.0
		if accumulatedSec >= (float64(m.segmentDuration) - 0.1) {
			shouldCutSeg = true
		}
	}

	if shouldCutSeg {
		// закрываем последний микро-чанк перед закрытием всего сегмента
		if len(m.partSamples) > 0 {
			m.flushPart()
		}
		m.flushSegment()

		// Добавляем текущий кадр в новые пустые массивы
		m.partSamples = append(m.partSamples, sample)
		m.segmentSamples = append(m.segmentSamples, sample)
		m.currentSegTime = uint64(sample.Duration)
		m.currentPartTime = uint64(sample.Duration)
		return
	}

	// если сегмент не рубим, проверяем, пора ли рубить микро-чанк (LL-HLS Part)
	shouldCutPart := false
	if len(m.partSamples) > 0 {
		accumulatedPartSec := float64(m.currentPartTime) / 90000.0
		if accumulatedPartSec >= m.partDurationSec {
			shouldCutPart = true
		}
	}

	if shouldCutPart {
		m.flushPart()
	}

	// просто копим кадры
	if isIDR || len(m.segmentSamples) > 0 {
		m.partSamples = append(m.partSamples, sample)
		m.segmentSamples = append(m.segmentSamples, sample)
		m.currentSegTime += uint64(sample.Duration)
		m.currentPartTime += uint64(sample.Duration)
	}
}

func (m *HLSMuxer) flushPart() {
	if len(m.partSamples) == 0 {
		return
	}

	m.partIndex++
	// имя микро-чанка: seg-1-part-1.m4s
	fileName := fmt.Sprintf("seg-%d-part-%d.m4s", m.segmentIndex+1, m.partIndex)
	filePath := filepath.Join(m.targetPath, fileName)

	part := &fmp4.Part{
		SequenceNumber: m.globalSeqNum,
		Tracks: []*fmp4.PartTrack{{
			ID:       1,
			BaseTime: m.partBaseTime, // время старта конкретно этого чанка
			Samples:  m.partSamples,
		}},
	}
	m.globalSeqNum++

	var partDuration uint64
	for _, s := range m.partSamples {
		partDuration += uint64(s.Duration)
	}
	m.partBaseTime += partDuration
	partDurationSec := float64(partDuration) / 90000.0

	file, err := os.Create(filePath)
	if err == nil {
		var buf seekablebuffer.Buffer
		part.Marshal(&buf)
		file.Write(buf.Bytes())
		file.Close()

		select {
		case m.UploadChan <- fileName:
		default:
		}
	}

	m.currentParts = append(m.currentParts, llPart{Name: fileName, Duration: partDurationSec})
	m.updatePlaylist()

	m.partSamples = make([]*fmp4.PartSample, 0)
	m.currentPartTime = 0
}

func (m *HLSMuxer) flushSegment() {
	if len(m.segmentSamples) == 0 {
		return
	}

	m.segmentIndex++
	fileName := fmt.Sprintf("seg-%d.m4s", m.segmentIndex)
	filePath := filepath.Join(m.targetPath, fileName)

	// склеиваем весь сегмент из накопленных кадров
	part := &fmp4.Part{
		SequenceNumber: m.globalSeqNum,
		Tracks: []*fmp4.PartTrack{{
			ID:       1,
			BaseTime: m.segBaseTime, // время старта ВСЕГО сегмента
			Samples:  m.segmentSamples,
		}},
	}
	m.globalSeqNum++

	var segDuration uint64
	for _, s := range m.segmentSamples {
		segDuration += uint64(s.Duration)
	}

	// сдвигаем базовое время для следующего сегмента
	m.segBaseTime += segDuration
	m.partBaseTime = m.segBaseTime // синхронизируем часы чанков с часами сегментов
	segDurationSec := float64(segDuration) / 90000.0

	file, err := os.Create(filePath)
	if err == nil {
		var buf seekablebuffer.Buffer
		part.Marshal(&buf)
		file.Write(buf.Bytes())
		file.Close()

		select {
		case m.UploadChan <- fileName:
		default:
		}
	}

	log.Printf("[LL-HLS] Segment %s closed (%.3f sec, contains %d parts)", fileName, segDurationSec, m.partIndex)

	m.segments = append(m.segments, segment{Name: fileName, Duration: segDurationSec})
	if len(m.segments) > 6 {
		m.segments = m.segments[1:]
	}

	// сегмент закрыт, микро-чанки этого сегмента нам больше не нужны
	m.currentParts = make([]llPart, 0)
	m.partIndex = 0

	m.updatePlaylist()

	m.segmentSamples = make([]*fmp4.PartSample, 0)
	m.currentSegTime = 0
}

func (m *HLSMuxer) writeInitSegment() {
	log.Println("[Muxer] Получены SPS/PPS. Генерируем init.mp4...")
	track := &fmp4.InitTrack{
		ID:        1,
		TimeScale: 90000,
		Codec: &fmp4.CodecH264{
			SPS: m.sps,
			PPS: m.pps,
		},
	}
	init := fmp4.Init{Tracks: []*fmp4.InitTrack{track}}
	file, err := os.Create(filepath.Join(m.targetPath, "init.mp4"))
	if err == nil {
		var buf seekablebuffer.Buffer
		init.Marshal(&buf)
		file.Write(buf.Bytes())
		file.Close()
		select {
		case m.UploadChan <- "init.mp4":
		default:
		}
	}
}

func (m *HLSMuxer) updatePlaylist() error {
	var maxDuration float64
	for _, seg := range m.segments {
		if seg.Duration > maxDuration {
			maxDuration = seg.Duration
		}
	}
	targetDuration := int(maxDuration + 0.999)
	if targetDuration < 1 {
		targetDuration = 1
	}

	sb := strings.Builder{}
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:7\n")

	// Server Control теги, это душа LL-HLS. Они говорят плееру, что можно запрашивать части.
	sb.WriteString(fmt.Sprintf("#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=%.3f\n", m.partDurationSec*2.5))
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	sb.WriteString(fmt.Sprintf("#EXT-X-PART-INF:PART-TARGET=%.3f\n", m.partDurationSec+0.05))

	sequence := m.segmentIndex - len(m.segments) + 1
	if sequence < 0 {
		sequence = 0
	}

	sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", sequence))
	sb.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	sb.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n\n")

	// сначала пишем уже готовые, большие сегменты
	for _, seg := range m.segments {
		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n%s\n", seg.Duration, seg.Name))
	}

	// а теперь дописываем свежие микро-части, которые прямо сейчас генерятся
	for _, part := range m.currentParts {
		sb.WriteString(fmt.Sprintf("#EXT-X-PART:DURATION=%.3f,URI=\"%s\"\n", part.Duration, part.Name))
	}

	tempFile := filepath.Join(m.targetPath, "index_tmp.m3u8")
	finalFile := filepath.Join(m.targetPath, "index.m3u8")
	os.WriteFile(tempFile, []byte(sb.String()), 0644)
	err := os.Rename(tempFile, finalFile)
	if err == nil {
		select {
		case m.UploadChan <- "index.m3u8":
		default:
		}
	}
	return err
}
