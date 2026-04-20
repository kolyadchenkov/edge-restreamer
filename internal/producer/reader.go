package producer

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
)

type RTSPReader struct {
	url    string
	cmd    *exec.Cmd
	stdout io.ReadCloser
}

func NewRTSPReader(url string) *RTSPReader {
	return &RTSPReader{url: url}
}

func (r *RTSPReader) Start(ctx context.Context) (io.Reader, error) {
	r.cmd = exec.CommandContext(ctx, "ffmpeg",
		"-rtsp_transport", "tcp",
		"-fflags", "+genpts",
		"-i", r.url,
		"-c:v", "copy", // Копируем видео без транскодирования
		"-bsf:v", "dump_extra",
		"-an",          // Отключаем аудио (если оно есть)
		"-f", "mpegts", // Формат на выходе TS
		"pipe:1", // Отдаем в stdout
	)

	// Перехватываем stdout
	stdout, err := r.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	r.stdout = stdout

	// Перенаправляем stderr в лог для дебага ffmpeg
	r.cmd.Stderr = os.Stderr

	if err := r.cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	log.Println("RTSP Reader started, ffmpeg PID:", r.cmd.Process.Pid)
	return r.stdout, nil
}

func (r *RTSPReader) Wait() error {
	if r.cmd != nil {
		return r.cmd.Wait()
	}
	return nil
}
