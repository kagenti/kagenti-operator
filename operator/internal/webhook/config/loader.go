package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

var log = logf.Log.WithName("config")

// ConfigLoader loads config from file and watches for changes
type ConfigLoader struct {
	configPath string

	// currentConfig stores an immutable *PlatformConfig snapshot.
	// Readers get the pointer directly (no copy), writers swap the
	// pointer atomically on each Load(). This avoids DeepCopy on every
	// webhook admission request.
	currentConfig atomic.Pointer[PlatformConfig]

	mu       sync.Mutex // protects onChange only
	onChange []func(*PlatformConfig)
}

func NewConfigLoader(configPath string) *ConfigLoader {
	l := &ConfigLoader{
		configPath: configPath,
	}
	l.currentConfig.Store(CompiledDefaults())
	return l
}

// Load reads config from file and merges with compiled defaults
func (l *ConfigLoader) Load() error {
	log.Info("Loading platform config", "path", l.configPath)

	// Start with compiled defaults (the ultimate fallback)
	config := CompiledDefaults()

	// Read config file
	data, err := os.ReadFile(l.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("Config file not found, using compiled defaults only")
			l.currentConfig.Store(config)
			l.mu.Lock()
			callbacks := make([]func(*PlatformConfig), len(l.onChange))
			copy(callbacks, l.onChange)
			l.mu.Unlock()
			logConfig(config, "compiled-defaults")
			for _, cb := range callbacks {
				cb(config.DeepCopy())
			}
			return nil
		}
		return err
	}

	// Parse YAML - this overlays onto the defaults
	// Fields not specified in file keep their compiled default values
	if err := yaml.Unmarshal(data, config); err != nil {
		return err
	}

	// Validate the merged config
	if err := config.Validate(); err != nil {
		return err
	}

	// Update current config (atomic swap — readers see it immediately)
	l.currentConfig.Store(config)

	log.Info("Platform config loaded successfully from file")
	logConfig(config, "configmap")

	// Snapshot callbacks under lock, then invoke outside lock
	// so callbacks can safely call Get() without deadlock.
	l.mu.Lock()
	callbacks := make([]func(*PlatformConfig), len(l.onChange))
	copy(callbacks, l.onChange)
	l.mu.Unlock()

	for _, cb := range callbacks {
		cb(config.DeepCopy())
	}

	return nil
}

// Get returns the current config snapshot (lock-free, no copy).
// The returned pointer must be treated as read-only; callers that need
// to mutate config should DeepCopy explicitly.
//
// NOTE: PlatformConfig contains reference-typed fields (Resources →
// corev1.ResourceRequirements with Limits/Requests maps). Struct-copying
// the returned value shares the underlying maps with the singleton.
// Never mutate map entries obtained through this pointer — doing so
// corrupts the shared config for all concurrent webhook requests.
func (l *ConfigLoader) Get() *PlatformConfig {
	return l.currentConfig.Load()
}

// Watch starts watching the config file for changes
func (l *ConfigLoader) Watch(ctx context.Context) error {
	// Watch the directory, not the file directly
	// ConfigMap volumes use symlinks that get replaced on update
	dir := filepath.Dir(l.configPath)

	// If the directory doesn't exist yet (e.g. volume not mounted),
	// skip watching — defaults are already loaded.
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		log.Info("Config directory not found, skipping watcher (using defaults)", "dir", dir)
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return err
	}

	log.Info("Watching config directory for changes", "dir", dir)

	go func() {
		defer func() { _ = watcher.Close() }()

		// Debounce rapid changes (ConfigMap updates can trigger multiple events)
		var debounceTimer *time.Timer
		defer func() {
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
		}()

		for {
			select {
			case <-ctx.Done():
				log.Info("Config watcher stopped")
				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				// ConfigMap updates create new symlinks
				if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove) != 0 {
					log.Info("Config change detected", "event", event.Name, "op", event.Op)

					// Debounce: wait 1 second before reloading
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					debounceTimer = time.AfterFunc(1*time.Second, func() {
						if err := l.Load(); err != nil {
							log.Error(err, "Failed to reload config")
						} else {
							log.Info("Config reloaded successfully")
						}
					})
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Error(err, "Config watcher error")
			}
		}
	}()

	return nil
}

// OnChange registers a callback for config changes.
// Safe to call concurrently with Load/Watch.
func (l *ConfigLoader) OnChange(cb func(*PlatformConfig)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onChange = append(l.onChange, cb)
}

// logConfig logs all configuration settings with the given source label
func logConfig(cfg *PlatformConfig, source string) {
	log.Info("========== PLATFORM CONFIGURATION ==========")
	log.Info("[config] source", "source", source)
	log.Info("[config] images",
		"envoyProxy", cfg.Images.EnvoyProxy,
		"authBridge", cfg.Images.AuthBridge,
		"authBridgeLite", cfg.Images.AuthBridgeLite,
		"proxyInit", cfg.Images.ProxyInit,
		"pullPolicy", cfg.Images.PullPolicy,
	)
	log.Info("[config] proxy",
		"port", cfg.Proxy.Port,
		"uid", cfg.Proxy.UID,
		"inboundProxyPort", cfg.Proxy.InboundProxyPort,
		"adminPort", cfg.Proxy.AdminPort,
		"transparentPort", cfg.Proxy.TransparentPort,
		"iptablesCmd", cfg.Proxy.IptablesCmd,
	)
	log.Info("[config] resources.envoyProxy",
		"requests", cfg.Resources.EnvoyProxy.Requests,
		"limits", cfg.Resources.EnvoyProxy.Limits,
	)
	log.Info("[config] resources.proxyInit",
		"requests", cfg.Resources.ProxyInit.Requests,
		"limits", cfg.Resources.ProxyInit.Limits,
	)
	log.Info("[config] resources.authBridge",
		"requests", cfg.Resources.AuthBridge.Requests,
		"limits", cfg.Resources.AuthBridge.Limits,
	)
	log.Info("[config] tokenExchange",
		"tokenUrl", cfg.TokenExchange.TokenURL,
		"defaultAudience", cfg.TokenExchange.DefaultAudience,
		"defaultScopes", cfg.TokenExchange.DefaultScopes,
	)
	helperSnippet := cfg.Spiffe.HelperConfig
	if len(helperSnippet) > 80 {
		helperSnippet = helperSnippet[:80] + "..."
	}
	log.Info("[config] spiffe",
		"trustDomain", cfg.Spiffe.TrustDomain,
		"socketPath", cfg.Spiffe.SocketPath,
		"helperConfig", helperSnippet,
	)
	log.Info("=============================================")
}
