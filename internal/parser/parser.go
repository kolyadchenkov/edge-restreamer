package parser

import (
	"bytes"
	"context"
	"io"
	"log"

	"github.com/asticode/go-astits"
)

type FrameType int

const (
	FrameTypeUnknown FrameType = iota
	FrameTypeIDR               // IDR (Ключевой кадр, начало gop)
	FrameTypeNonIDR            // Обычный P/B кадр
)

type VideoFrame struct {
	Data       []byte    // Сырые байты h.264
	PTS        int64     // Время показа
	DTS        int64     // Время докодирования
	Type       FrameType // Тип кадра
	IsGOPStart bool      // Флажок для удобства при IDR

	// складываем ДНК видео
	SPS []byte
	PPS []byte
}

type GOPParser struct {
	reader *astits.Demuxer
	out    chan VideoFrame // канал, куда будем выплевывать готовые кадры
}

func NewGOPParser(r io.Reader) *GOPParser {
	return &GOPParser{
		// инициализация ts-демуксера
		reader: astits.NewDemuxer(context.Background(), r, astits.DemuxerOptPacketSize(188)),
		out:    make(chan VideoFrame, 100), // буфф канал на 100 кадров
	}
}

// запускает бесконечный цикл разбора потока
func (p *GOPParser) Start(ctx context.Context) {
	defer close(p.out)
	log.Println("Starting GOP Parser")

	for {
		select {
		case <-ctx.Done():
			log.Println("GOP Parser stopped")
			return
		default:
			// читаем в следующий блок данных из ts
			data, err := p.reader.NextData()
			if err != nil {
				if err == astits.ErrNoMorePackets {
					log.Println("EOF reached in TS steam")
					return
				}
				log.Printf("Error reading TS data: %v", err)
				continue
			}

			// интересуют только pes-пакеты (где лежит само видео)
			if data.PES == nil || data.PES.Header == nil {
				continue
			}

			// проверяем, что это видео (stream_id начинается с 0xE0 для видер)
			if data.PES.Header.StreamID >= 0xE0 && data.PES.Header.StreamID <= 0xEF {
				frame := p.parseH264PES(data.PES)

				// отправляем кадр в канал (дальше его заберут HLS и SRT)
				p.out <- frame
			}
		}
	}
}

// возвращаем в канал для чтения кадров
func (p *GOPParser) Out() <-chan VideoFrame {
	return p.out
}

// ковыряет байты h.264 чтобы найти idr кадры
func (p *GOPParser) parseH264PES(pes *astits.PESData) VideoFrame {
	frame := VideoFrame{
		Data: pes.Data,
		Type: FrameTypeUnknown,
	}

	// достаем таймстемпы если есть
	if pes.Header.OptionalHeader != nil {
		if pes.Header.OptionalHeader.PTS != nil {
			frame.PTS = pes.Header.OptionalHeader.PTS.Base
		}
		if pes.Header.OptionalHeader.DTS != nil {
			frame.DTS = pes.Header.OptionalHeader.DTS.Base
		} else {
			frame.DTS = frame.PTS // dts часто совпадает с pts
		}
	}

	// Ищем все NALU в пакете в цикле
	data := pes.Data
	startCode3 := []byte{0, 0, 1}
	startCode4 := []byte{0, 0, 0, 1}

	for len(data) > 3 {
		// ищем начало текущего nalu
		idx := bytes.Index(data, startCode4)
		offset := 4
		if idx == -1 {
			idx = bytes.Index(data, startCode3)
			offset = 3
		}

		if idx == -1 {
			break // больше маркеров нет, выходим из цикла
		}

		naluOffset := idx + offset
		if len(data) <= naluOffset {
			break
		}

		// вычисляем тип (первые 5 бит)
		naluType := data[naluOffset] & 0x1F

		// ищем начало следующего nalu, чтобы понять длину текущего
		nextIdx := bytes.Index(data[naluOffset:], startCode4)
		if nextIdx == -1 {
			nextIdx = bytes.Index(data[naluOffset:], startCode3)
		}

		// вырезаем сами байты nalu (без стартовых нулей)
		var naluData []byte
		if nextIdx == -1 {
			naluData = data[naluOffset:] // это последний кусок в пакете
		} else {
			naluData = data[naluOffset : naluOffset+nextIdx] // вырезаем строго до следующего маркера
		}

		// раскладываем по полочкам
		switch naluType {
		case 7: // SPS
			frame.SPS = make([]byte, len(naluData))
			copy(frame.SPS, naluData) // копируем байты, чтобы сборщик fMP4 смог их прочитать
		case 8: // PPS
			frame.PPS = make([]byte, len(naluData))
			copy(frame.PPS, naluData)
		case 5: // IDR
			frame.Type = FrameTypeIDR
			frame.IsGOPStart = true
		case 1: // Обычный кадр
			frame.Type = FrameTypeNonIDR
		}

		// сдвигаем окно поиска вперед, чтобы на следующей итерации искать дальше
		if nextIdx == -1 {
			break
		}
		data = data[naluOffset+nextIdx:]
	}

	// если мы нашли IDR, а перед ним в этом же пакете были SPS, PPS - помечаем весь пакет как начало GOP
	if frame.Type == FrameTypeIDR {
		frame.IsGOPStart = true
	}

	return frame
}
