package controller

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
	"github.com/andreabedini/minecraft-operator/internal/msmp"
	"github.com/andreabedini/minecraft-operator/internal/plan"
	"github.com/andreabedini/minecraft-operator/internal/supervisorclient"
)

const (
	managementQueryTimeout = 10 * time.Second
	watcherMinBackoff      = 2 * time.Second
	watcherMaxBackoff      = time.Minute
)

// Prometheus metrics per instance, labelled by namespace and name.
var (
	metricStarted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "minecraft_instance_started",
		Help: "1 when the management protocol reports the server started.",
	}, []string{"namespace", "name"})
	metricPlayersOnline = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "minecraft_instance_players_online",
		Help: "Players currently connected.",
	}, []string{"namespace", "name"})
	metricLastSave = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "minecraft_instance_last_save_timestamp_seconds",
		Help: "Unix time of the last completed world save.",
	}, []string{"namespace", "name"})
	metricUpgradeProgress = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "minecraft_instance_world_upgrade_progress",
		Help: "World upgrade progress from 0 to 1; 0 when no upgrade runs.",
	}, []string{"namespace", "name"})
	metricPlayerJoins = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "minecraft_instance_player_joins_total",
		Help: "Player join notifications received.",
	}, []string{"namespace", "name"})
)

func init() {
	metrics.Registry.MustRegister(metricStarted, metricPlayersOnline, metricLastSave, metricUpgradeProgress, metricPlayerJoins)
}

// liveState is what the notification watcher has learned since the last
// reconcile. The reconciler copies it into status.
type liveState struct {
	mu              sync.Mutex
	players         *int32
	lastSave        *time.Time
	upgradePhase    string
	upgradeProgress int32
}

func (l *liveState) snapshot() (players *int32, lastSave *time.Time, upgradePhase string, upgradeProgress int32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.players, l.lastSave, l.upgradePhase, l.upgradeProgress
}

// managementDialer returns a Dialer over the supervisor tunnel.
func managementDialer(sup *supervisorclient.Client) msmp.Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		return sup.DialTunnel(ctx, plan.ManagementTunnelTarget)
	}
}

// queryManagement asks the running server for its state.
func queryManagement(ctx context.Context, sup *supervisorclient.Client, secret string) (*msmp.ServerState, string, error) {
	ctx, cancel := context.WithTimeout(ctx, managementQueryTimeout)
	defer cancel()
	c, err := msmp.Dial(ctx, managementDialer(sup), secret)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = c.Close() }()
	st, err := c.Status(ctx)
	if err != nil {
		return nil, "", err
	}
	version, err := c.ProtocolVersion(ctx)
	if err != nil {
		// Old servers may lack rpc.discover; the state is still useful.
		version = ""
	}
	return st, version, nil
}

// watcher keeps a management connection open for one instance and turns
// notifications into events, metrics and reconcile triggers.
type watcher struct {
	key    string
	cancel context.CancelFunc
	live   *liveState
}

// watcherKey identifies a watcher's target: a new pod or a new secret means
// a new connection.
func watcherKey(pod *corev1.Pod, secret string) string {
	return string(pod.UID) + "/" + secret
}

// ensureWatcher starts or replaces the watcher for an instance.
func (r *MinecraftInstanceReconciler) ensureWatcher(inst *v1alpha1.MinecraftInstance, pod *corev1.Pod, sup *supervisorclient.Client, secret string) *liveState {
	nn := types.NamespacedName{Namespace: inst.Namespace, Name: inst.Name}
	key := watcherKey(pod, secret)
	r.watchersMu.Lock()
	defer r.watchersMu.Unlock()
	if r.watchers == nil {
		r.watchers = map[types.NamespacedName]*watcher{}
	}
	if w, ok := r.watchers[nn]; ok {
		if w.key == key {
			return w.live
		}
		w.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{key: key, cancel: cancel, live: &liveState{}}
	r.watchers[nn] = w
	go r.watch(ctx, nn, inst.UID, sup, secret, w.live)
	return w.live
}

// stopWatcher ends the watcher for an instance, if any.
func (r *MinecraftInstanceReconciler) stopWatcher(nn types.NamespacedName) {
	r.watchersMu.Lock()
	defer r.watchersMu.Unlock()
	if w, ok := r.watchers[nn]; ok {
		w.cancel()
		delete(r.watchers, nn)
	}
	labels := prometheus.Labels{"namespace": nn.Namespace, "name": nn.Name}
	metricStarted.Delete(labels)
	metricPlayersOnline.Delete(labels)
	metricLastSave.Delete(labels)
	metricUpgradeProgress.Delete(labels)
	metricPlayerJoins.Delete(labels)
}

// requeue asks the controller to reconcile the instance soon.
func (r *MinecraftInstanceReconciler) requeue(nn types.NamespacedName, uid types.UID) {
	if r.triggers == nil {
		return
	}
	obj := &v1alpha1.MinecraftInstance{}
	obj.Namespace, obj.Name, obj.UID = nn.Namespace, nn.Name, uid
	select {
	case r.triggers <- event.TypedGenericEvent[client.Object]{Object: obj}:
	default:
	}
}

func (r *MinecraftInstanceReconciler) watch(ctx context.Context, nn types.NamespacedName, uid types.UID, sup *supervisorclient.Client, secret string, live *liveState) {
	logger := log.Log.WithName("management").WithValues("instance", nn.String())
	labels := prometheus.Labels{"namespace": nn.Namespace, "name": nn.Name}
	backoff := watcherMinBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		dialCtx, cancel := context.WithTimeout(ctx, managementQueryTimeout)
		c, err := msmp.Dial(dialCtx, managementDialer(sup), secret)
		cancel()
		if err != nil {
			logger.V(1).Info("management connection failed, retrying", "error", err.Error(), "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, watcherMaxBackoff)
			continue
		}
		backoff = watcherMinBackoff
		logger.Info("management connection established")
		r.consume(ctx, c, nn, uid, labels, live)
		_ = c.Close()
		metricStarted.With(labels).Set(0)
	}
}

func (r *MinecraftInstanceReconciler) consume(ctx context.Context, c *msmp.Client, nn types.NamespacedName, uid types.UID, labels prometheus.Labels, live *liveState) {
	logger := log.Log.WithName("management").WithValues("instance", nn.String())
	refresh := func() {
		qctx, cancel := context.WithTimeout(ctx, managementQueryTimeout)
		defer cancel()
		st, err := c.Status(qctx)
		if err != nil {
			return
		}
		n := int32(len(st.Players))
		live.mu.Lock()
		live.players = &n
		live.mu.Unlock()
		metricPlayersOnline.With(labels).Set(float64(n))
		if st.Started {
			metricStarted.With(labels).Set(1)
		} else {
			metricStarted.With(labels).Set(0)
		}
	}
	refresh()
	r.requeue(nn, uid)

	eventObj := &v1alpha1.MinecraftInstance{}
	eventObj.Namespace, eventObj.Name, eventObj.UID = nn.Namespace, nn.Name, uid
	eventObj.Kind, eventObj.APIVersion = "MinecraftInstance", v1alpha1.GroupVersion.String()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Done():
			logger.Info("management connection closed", "error", errString(c.Err()))
			return
		case n, ok := <-c.Notifications():
			if !ok {
				return
			}
			switch n.Method {
			case msmp.NotifyPlayerJoined, msmp.NotifyPlayerLeft:
				var p msmp.Player
				_ = n.FirstParam(&p)
				if n.Method == msmp.NotifyPlayerJoined {
					metricPlayerJoins.With(labels).Inc()
					r.Recorder.Eventf(eventObj, corev1.EventTypeNormal, "PlayerJoined", "%s joined", p.Name)
				} else {
					r.Recorder.Eventf(eventObj, corev1.EventTypeNormal, "PlayerLeft", "%s left", p.Name)
				}
				refresh()
				r.requeue(nn, uid)
			case msmp.NotifyServerStarted:
				r.Recorder.Event(eventObj, corev1.EventTypeNormal, "ServerStarted", "server reports started")
				refresh()
				r.requeue(nn, uid)
			case msmp.NotifyServerStopping:
				r.Recorder.Event(eventObj, corev1.EventTypeNormal, "ServerStopping", "server reports stopping")
				metricStarted.With(labels).Set(0)
				r.requeue(nn, uid)
			case msmp.NotifyServerSaved:
				now := time.Now()
				live.mu.Lock()
				live.lastSave = &now
				live.mu.Unlock()
				metricLastSave.With(labels).Set(float64(now.Unix()))
			case msmp.NotifyServerStatus:
				var st msmp.ServerState
				if n.FirstParam(&st) == nil {
					cnt := int32(len(st.Players))
					live.mu.Lock()
					live.players = &cnt
					live.mu.Unlock()
					metricPlayersOnline.With(labels).Set(float64(cnt))
					if st.Started {
						metricStarted.With(labels).Set(1)
					}
				}
			case msmp.NotifyUpgradeStarted:
				r.Recorder.Event(eventObj, corev1.EventTypeNormal, "WorldUpgradeStarted", "world upgrade started")
				live.mu.Lock()
				live.upgradePhase, live.upgradeProgress = "Upgrading", 0
				live.mu.Unlock()
				r.requeue(nn, uid)
			case msmp.NotifyUpgradeProg:
				var p struct {
					Progress float64 `json:"progress"`
				}
				if n.FirstParam(&p) == nil {
					pct := int32(p.Progress * 100)
					live.mu.Lock()
					changed := pct/5 != live.upgradeProgress/5
					live.upgradePhase, live.upgradeProgress = "Upgrading", pct
					live.mu.Unlock()
					metricUpgradeProgress.With(labels).Set(p.Progress)
					if changed {
						r.requeue(nn, uid)
					}
				}
			case msmp.NotifyUpgradeDone, msmp.NotifyUpgradeFailed:
				phase := "Finished"
				if n.Method == msmp.NotifyUpgradeFailed {
					phase = "Failed"
					r.Recorder.Event(eventObj, corev1.EventTypeWarning, "WorldUpgradeFailed", "world upgrade failed")
				} else {
					r.Recorder.Event(eventObj, corev1.EventTypeNormal, "WorldUpgradeFinished", "world upgrade finished")
				}
				live.mu.Lock()
				live.upgradePhase, live.upgradeProgress = phase, 100
				live.mu.Unlock()
				metricUpgradeProgress.With(labels).Set(0)
				r.requeue(nn, uid)
			}
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// managementUnavailable formats a dial or query failure for a condition.
func managementUnavailable(err error) string {
	return fmt.Sprintf("management protocol not reachable yet: %s", trimErr(err))
}
