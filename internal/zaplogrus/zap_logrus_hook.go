package zaplogrus

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type ZapLogrusHook struct {
	logger *zap.Logger
}

func NewZapLogrusHook(logger *zap.Logger) *ZapLogrusHook {
	return &ZapLogrusHook{
		logger: logger,
	}
}

func (h *ZapLogrusHook) Fire(entry *logrus.Entry) error {
	zapLevel := h.zapLevel(entry.Level)
	if zapLevel == zapcore.InvalidLevel {
		return fmt.Errorf("unhandled logrus level %v", entry.Level)
	}

	if ce := h.logger.Check(zapLevel, entry.Message); ce != nil {
		caller := entry.Caller

		if caller != nil {
			ce.Caller = zapcore.NewEntryCaller(caller.PC, caller.File, caller.Line, caller.PC != 0)
		}

		fields := make([]zap.Field, 0, len(entry.Data))

		for key, value := range entry.Data {
			if key == logrus.ErrorKey {
				fields = append(fields, zap.Error(value.(error)))
			} else {
				fields = append(fields, zap.Any(key, value))
			}
		}

		ce.Write(fields...)
	}

	return nil
}

func (h *ZapLogrusHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *ZapLogrusHook) zapLevel(level logrus.Level) zapcore.Level {
	switch level {
	case logrus.PanicLevel:
		return zapcore.PanicLevel
	case logrus.FatalLevel:
		return zapcore.FatalLevel
	case logrus.ErrorLevel:
		return zapcore.ErrorLevel
	case logrus.WarnLevel:
		return zapcore.WarnLevel
	case logrus.InfoLevel:
		return zapcore.InfoLevel
	case logrus.DebugLevel, logrus.TraceLevel:
		return zapcore.DebugLevel
	default:
		return zapcore.InvalidLevel
	}
}
