package cluster

import (
	"context"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/k3s-io/k3s/pkg/clientaccess"
	"github.com/k3s-io/k3s/pkg/cluster/managed"
	"github.com/k3s-io/k3s/pkg/daemons/config"
	"github.com/k3s-io/k3s/pkg/etcd"
	"github.com/k3s-io/kine/pkg/endpoint"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"
	utilsnet "k8s.io/utils/net"
)

type Cluster struct {
	clientAccessInfo *clientaccess.Info
	config           *config.Control
	managedDB        managed.Driver
	joining          bool
	storageStarted   bool
	saveBootstrap    bool
	cnFilterFunc     func(...string) []string
}

// Start creates the dynamic tls listener, http request handler,
// handles starting and writing/reading bootstrap data, and returns a channel
// that will be closed when datastore is ready. If embedded etcd is in use,
// a secondary call to Cluster.save is made.
func (c *Cluster) Start(ctx context.Context) (<-chan struct{}, error) {
	// Set up the dynamiclistener and http request handlers
	if err := c.initClusterAndHTTPS(ctx); err != nil {
		return nil, errors.Wrap(err, "init cluster datastore and https")
	}

	if c.config.DisableETCD {
		ready := make(chan struct{})
		defer close(ready)
		return ready, nil
	}

	// start managed database (if necessary)
	if err := c.start(ctx); err != nil {
		return nil, errors.Wrap(err, "start managed database")
	}

	// get the wait channel for testing managed database readiness
	ready, err := c.testClusterDB(ctx)
	if err != nil {
		return nil, err
	}

	if err := c.startStorage(ctx, false); err != nil {
		return nil, err
	}

	// if necessary, store bootstrap data to datastore
	if c.saveBootstrap {
		if err := Save(ctx, c.config, false); err != nil {
			return nil, err
		}
	}

	// at this point, if etcd is in use, it's bootstrapping is complete
	// so save the bootstrap data. We will need for etcd to be up. If
	// the save call returns an error, we panic since subsequent etcd
	// snapshots will be empty.
	if c.managedDB != nil {
		go func() {
			for {
				select {
				case <-ready:
					if err := Save(ctx, c.config, false); err != nil {
						panic(err)
					}

					if !c.config.EtcdDisableSnapshots {
						_ = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
							err := c.managedDB.ReconcileSnapshotData(ctx)
							if err != nil {
								logrus.Errorf("Failed to record snapshots for cluster: %v", err)
							}
							return err == nil, nil
						})
					}
					return
				default:
					runtime.Gosched()
				}
			}
		}()
	}

	return ready, nil
}

// startEtcdProxy starts an etcd load-balancer proxy, for control-plane-only nodes
// without a local datastore.
func (c *Cluster) startEtcdProxy(ctx context.Context) error {
	defaultURL, err := url.Parse(c.config.JoinURL)
	if err != nil {
		return err
	}
	defaultURL.Host = defaultURL.Hostname() + ":2379"
	etcdProxy, err := etcd.NewETCDProxy(ctx, c.config.SupervisorPort, c.config.DataDir, defaultURL.String(), utilsnet.IsIPv6CIDR(c.config.ServiceIPRanges[0]))
	if err != nil {
		return err
	}

	// immediately update the load balancer with all etcd addresses
	// from /db/info, for a current list of etcd cluster member client URLs.
	// client URLs are a full URI, but the proxy only wants host:port
	if clientURLs, _, err := etcd.ClientURLs(ctx, c.clientAccessInfo, c.config.PrivateIP); err != nil || len(clientURLs) == 0 {
		logrus.Warnf("Failed to get etcd ClientURLs: %v", err)
	} else {
		for i, c := range clientURLs {
			u, err := url.Parse(c)
			if err != nil {
				return errors.Wrap(err, "failed to parse etcd ClientURL")
			}
			clientURLs[i] = u.Host
		}
		etcdProxy.Update(clientURLs)
	}

	// start periodic endpoint sync goroutine
	c.setupEtcdProxy(ctx, etcdProxy)

	// remove etcd member if it exists
	if err := c.managedDB.RemoveSelf(ctx); err != nil {
		logrus.Warnf("Failed to remove this node from etcd members: %v", err)
	}

	c.config.Runtime.EtcdConfig.Endpoints = strings.Split(c.config.Datastore.Endpoint, ",")
	c.config.Runtime.EtcdConfig.TLSConfig = c.config.Datastore.BackendTLSConfig

	return nil
}

// startStorage starts the kine listener and configures the endpoints, if necessary.
// This calls into the kine endpoint code, which sets up the database client
// and unix domain socket listener if using an external database. In the case of an etcd
// backend it just returns the user-provided etcd endpoints and tls config.
func (c *Cluster) startStorage(ctx context.Context, bootstrap bool) error {
	if c.storageStarted && !c.config.KineTLS {
		return nil
	}
	c.storageStarted = true

	if !bootstrap {
		// set the tls config for the kine storage
		c.config.Datastore.ServerTLSConfig.CAFile = c.config.Runtime.ETCDServerCA
		c.config.Datastore.ServerTLSConfig.CertFile = c.config.Runtime.ServerETCDCert
		c.config.Datastore.ServerTLSConfig.KeyFile = c.config.Runtime.ServerETCDKey
	}

	// start listening on the kine socket as an etcd endpoint, or return the external etcd endpoints
	etcdConfig, err := endpoint.Listen(ctx, c.config.Datastore)
	if err != nil {
		return errors.Wrap(err, "creating storage endpoint")
	}

	// Persist the returned etcd configuration. We decide if we're doing leader election for embedded controllers
	// based on what the kine wrapper tells us about the datastore. Single-node datastores like sqlite don't require
	// leader election, while basically all others (etcd, external database, etc) do since they allow multiple servers.
	c.config.Runtime.EtcdConfig = etcdConfig

	// after the bootstrap we need to set the args for api-server with kine in unixs or just set the
	// values if the datastoreTLS is not enabled
	if !bootstrap || !c.config.KineTLS {
		c.config.Datastore.BackendTLSConfig = etcdConfig.TLSConfig
		c.config.Datastore.Endpoint = strings.Join(etcdConfig.Endpoints, ",")
		c.config.NoLeaderElect = !etcdConfig.LeaderElect
	}

	return nil
}

// New creates an initial cluster using the provided configuration.
func New(config *config.Control) *Cluster {
	return &Cluster{
		config: config,
	}
}
