package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"sigs.k8s.io/yaml"
)

// FeatureGateLoader loads feature gates from file and watches for changes.
type FeatureGateLoader struct {
	configPath string

	current atomic.Pointer[FeatureGates]

	mu       sync.Mutex // protects onChange only
	onChange []func(*FeatureGates)
}

func NewFeatureGateLoader(configPath string) *FeatureGateLoader {
	l := &FeatureGateLoader{
		configPath: configPath,
	}
	l.current.Store(DefaultFeatureGates())
	return l
}

// Load reads feature gates from file.
func (l *FeatureGateLoader) Load() error {
	log.Info("Loading feature gates", "path", l.configPath)

	gates := DefaultFeatureGates()

	data, err := os.ReadFile(l.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("Feature gates file not found, using defaults (all enabled)")
			l.current.Store(gates)
			l.mu.Lock()
			callbacks := make([]func(*FeatureGates), len(l.onChange))
			copy(callbacks, l.onChange)
			l.mu.Unlock()
			logFeatureGates(gates, "compiled-defaults")
			for _, cb := range callbacks {
				cb(gates.DeepCopy())
			}
			return nil
		}
		return fmt.Errorf("reading feature gates file %s: %w", l.configPath, err)
	}

	if err := yaml.Unmarshal(data, gates); err != nil {
		return fmt.Errorf("parsing feature gates file %s: %w", l.configPath, err)
	}

	// Guard against an empty or all-zero feature gates file (e.g. `featureGates`
	// key removed from values.yaml renders `{}` which unmarshals to all booleans
	// false). Fall back to compiled defaults so injection is not silently disabled.
	if *gates == (FeatureGates{}) {
		log.Info("Feature gates file is empty or all-zero, using compiled defaults to avoid silently disabling injection")
		gates = DefaultFeatureGates()
	}

	l.current.Store(gates)

	logFeatureGates(gates, "configmap")

	// Snapshot callbacks under lock, then invoke outside lock
	// so callbacks can safely call Get() without deadlock.
	l.mu.Lock()
	callbacks := make([]func(*FeatureGates), len(l.onChange))
	copy(callbacks, l.onChange)
	l.mu.Unlock()

	for _, cb := range callbacks {
		cb(gates.DeepCopy())
	}

	return nil
}

// Get returns the current feature gates snapshot (lock-free, no copy).
// FeatureGates contains only value types (booleans), so sharing is safe.
func (l *FeatureGateLoader) Get() *FeatureGates {
	return l.current.Load()
}

// Watch starts watching the feature gates file for changes.
func (l *FeatureGateLoader) Watch(ctx context.Context) error {
	dir := filepath.Dir(l.configPath)

	// If the directory doesn't exist yet (e.g. volume not mounted),
	// skip watching — defaults are already loaded.
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		log.Info("Feature gates directory not found, skipping watcher (using defaults)", "dir", dir)
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating feature gates file watcher: %w", err)
	}

	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watching feature gates directory %s: %w", dir, err)
	}

	log.Info("Watching feature gates directory for changes", "dir", dir)

	go func() {
		defer func() { _ = watcher.Close() }()

		var debounceTimer *time.Timer
		defer func() {
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
		}()

		for {
			select {
			case <-ctx.Done():
				log.Info("Feature gates watcher stopped")
				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove) != 0 {
					log.Info("Feature gates change detected", "event", event.Name, "op", event.Op)

					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					debounceTimer = time.AfterFunc(1*time.Second, func() {
						if err := l.Load(); err != nil {
							log.Error(err, "Failed to reload feature gates")
						} else {
							log.Info("Feature gates reloaded successfully")
						}
					})
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Error(err, "Feature gates watcher error")
			}
		}
	}()

	return nil
}

// OnChange registers a callback for feature gate changes.
// Safe to call concurrently with Load/Watch.
func (l *FeatureGateLoader) OnChange(cb func(*FeatureGates)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onChange = append(l.onChange, cb)
}

// logFeatureGates logs feature gate settings with a visible banner.
func logFeatureGates(fg *FeatureGates, source string) {
	log.Info("============= FEATURE GATES ================")
	log.Info("[feature-gates] source", "source", source)
	log.Info("[feature-gates] gates",
		"globalEnabled", fg.GlobalEnabled,
		"envoyProxy", fg.EnvoyProxy,
		"injectTools", fg.InjectTools,
		"perWorkloadConfigResolution", fg.PerWorkloadConfigResolution,
		"skillDiscovery", fg.SkillDiscovery,
	)
	log.Info("=============================================")
}
