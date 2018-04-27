package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"

	//	_ "net/http/pprof"

	logging "github.com/ipfs/go-log"
	ma "github.com/multiformats/go-multiaddr"
	cli "github.com/urfave/cli"

	ipfscluster "github.com/ipfs/ipfs-cluster"
	"github.com/ipfs/ipfs-cluster/allocator/ascendalloc"
	"github.com/ipfs/ipfs-cluster/allocator/descendalloc"
	"github.com/ipfs/ipfs-cluster/api/rest"
	"github.com/ipfs/ipfs-cluster/config"
	"github.com/ipfs/ipfs-cluster/consensus/raft"
	"github.com/ipfs/ipfs-cluster/informer/disk"
	"github.com/ipfs/ipfs-cluster/informer/numpin"
	"github.com/ipfs/ipfs-cluster/ipfsconn/ipfshttp"
	"github.com/ipfs/ipfs-cluster/monitor/basic"
	"github.com/ipfs/ipfs-cluster/pintracker/maptracker"
	"github.com/ipfs/ipfs-cluster/sharder"
	"github.com/ipfs/ipfs-cluster/state/mapstate"
)

// ProgramName of this application
const programName = `ipfs-cluster-service`

// We store a commit id here
var commit string

// Description provides a short summary of the functionality of this tool
var Description = fmt.Sprintf(`
%s runs an IPFS Cluster node.

A node participates in the cluster consensus, follows a distributed log
of pinning and unpinning requests and manages pinning operations to a
configured IPFS daemon.

This node also provides an API for cluster management, an IPFS Proxy API which
forwards requests to IPFS and a number of components for internal communication
using LibP2P. This is a simplified view of the components:

             +------------------+
             | ipfs-cluster-ctl |
             +---------+--------+
                       |
                       | HTTP(s)
ipfs-cluster-service   |                           HTTP
+----------+--------+--v--+----------------------+      +-------------+
| RPC/Raft | Peer 1 | API | IPFS Connector/Proxy +------> IPFS daemon |
+----^-----+--------+-----+----------------------+      +-------------+
     | libp2p
     |
+----v-----+--------+-----+----------------------+      +-------------+
| RPC/Raft | Peer 2 | API | IPFS Connector/Proxy +------> IPFS daemon |
+----^-----+--------+-----+----------------------+      +-------------+
     |
     |
+----v-----+--------+-----+----------------------+      +-------------+
| RPC/Raft | Peer 3 | API | IPFS Connector/Proxy +------> IPFS daemon |
+----------+--------+-----+----------------------+      +-------------+


%s needs a valid configuration to run. This configuration is
independent from IPFS and includes its own LibP2P key-pair. It can be
initialized with "init" and its default location is
 ~/%s/%s.

For feedback, bug reports or any additional information, visit
https://github.com/ipfs/ipfs-cluster.


EXAMPLES

Initial configuration:

$ ipfs-cluster-service init

Launch a cluster:

$ ipfs-cluster-service

Launch a peer and join existing cluster:

$ ipfs-cluster-service --bootstrap /ip4/192.168.1.2/tcp/9096/ipfs/QmPSoSaPXpyunaBwHs1rZBKYSqRV4bLRk32VGYLuvdrypL
`,
	programName,
	programName,
	DefaultPath,
	DefaultConfigFile)

var logger = logging.Logger("service")

// Default location for the configurations and data
var (
	// DefaultPath is initialized to $HOME/.ipfs-cluster
	// and holds all the ipfs-cluster data
	DefaultPath string
	// The name of the configuration file inside DefaultPath
	DefaultConfigFile = "service.json"
)

var (
	configPath string
)

func init() {
	// Set the right commit. The only way I could make this work
	ipfscluster.Commit = commit

	// We try guessing user's home from the HOME variable. This
	// allows HOME hacks for things like Snapcraft builds. HOME
	// should be set in all UNIX by the OS. Alternatively, we fall back to
	// usr.HomeDir (which should work on Windows etc.).
	home := os.Getenv("HOME")
	if home == "" {
		usr, err := user.Current()
		if err != nil {
			panic(fmt.Sprintf("cannot get current user: %s", err))
		}
		home = usr.HomeDir
	}

	DefaultPath = filepath.Join(home, ".ipfs-cluster")
}

func out(m string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, m, a...)
}

func checkErr(doing string, err error, args ...interface{}) {
	if err != nil {
		if len(args) > 0 {
			doing = fmt.Sprintf(doing, args)
		}
		out("error %s: %s\n", doing, err)
		err = locker.tryUnlock()
		if err != nil {
			out("error releasing execution lock: %s\n", err)
		}
		os.Exit(1)
	}
}

func main() {
	// go func() {
	//	log.Println(http.ListenAndServe("localhost:6060", nil))
	// }()

	app := cli.NewApp()
	app.Name = programName
	app.Usage = "IPFS Cluster node"
	app.Description = Description
	//app.Copyright = "© Protocol Labs, Inc."
	app.Version = ipfscluster.Version
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "config, c",
			Value:  DefaultPath,
			Usage:  "path to the configuration and data `FOLDER`",
			EnvVar: "IPFS_CLUSTER_PATH",
		},
		cli.BoolFlag{
			Name:  "force, f",
			Usage: "forcefully proceed with some actions. i.e. overwriting configuration",
		},
		cli.StringFlag{
			Name:  "bootstrap, j",
			Usage: "join a cluster providing an existing peer's `multiaddress`. Overrides the \"bootstrap\" values from the configuration",
		},
		cli.BoolFlag{
			Name:   "leave, x",
			Usage:  "remove peer from cluster on exit. Overrides \"leave_on_shutdown\"",
			Hidden: true,
		},
		cli.BoolFlag{
			Name:  "debug, d",
			Usage: "enable full debug logging (very verbose)",
		},
		cli.StringFlag{
			Name:  "loglevel, l",
			Value: "info",
			Usage: "set the loglevel for cluster components only [critical, error, warning, info, debug]",
		},
		cli.StringFlag{
			Name:  "alloc, a",
			Value: "disk-freespace",
			Usage: "allocation strategy to use [disk-freespace,disk-reposize,numpin].",
		},
	}

	app.Commands = []cli.Command{
		{
			Name:  "init",
			Usage: "create a default configuration and exit",
			Description: fmt.Sprintf(`
This command will initialize a new service.json configuration file
for %s.

By default, %s requires a cluster secret. This secret will be
automatically generated, but can be manually provided with --custom-secret
(in which case it will be prompted), or by setting the CLUSTER_SECRET
environment variable.

The private key for the libp2p node is randomly generated in all cases.

Note that the --force first-level-flag allows to overwrite an existing
configuration.
`, programName, programName),
			ArgsUsage: " ",
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:  "custom-secret, s",
					Usage: "prompt for the cluster secret",
				},
			},
			Action: func(c *cli.Context) error {
				userSecret, userSecretDefined := userProvidedSecret(c.Bool("custom-secret"))

				cfgMgr, cfgs := makeConfigs()
				defer cfgMgr.Shutdown() // wait for saves

				// Generate defaults for all registered components
				err := cfgMgr.Default()
				checkErr("generating default configuration", err)

				// Set user secret
				if userSecretDefined {
					cfgs.clusterCfg.Secret = userSecret
				}

				// Save
				saveConfig(cfgMgr, c.GlobalBool("force"))
				return nil
			},
		},
		{
			Name:  "daemon",
			Usage: "run the IPFS Cluster peer (default)",
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:  "upgrade, u",
					Usage: "run necessary state migrations before starting cluster service",
				},
			},
			Action: daemon,
		},
		{
			Name:  "state",
			Usage: "Manage ipfs-cluster-state",
			Subcommands: []cli.Command{
				{
					Name:  "version",
					Usage: "display the shared state format version",
					Action: func(c *cli.Context) error {
						fmt.Printf("%d\n", mapstate.Version)
						return nil
					},
				},
				{
					Name:  "upgrade",
					Usage: "upgrade the IPFS Cluster state to the current version",
					Description: `
This command upgrades the internal state of the ipfs-cluster node 
specified in the latest raft snapshot. The state format is migrated from the 
version of the snapshot to the version supported by the current cluster version. 
To successfully run an upgrade of an entire cluster, shut down each peer without
removal, upgrade state using this command, and restart every peer.
`,
					Action: func(c *cli.Context) error {
						err := locker.lock()
						checkErr("acquiring execution lock", err)
						defer locker.tryUnlock()

						err = upgrade()
						checkErr("upgrading state", err)
						return nil
					},
				},
				{
					Name:  "export",
					Usage: "save the IPFS Cluster state to a json file",
					Description: `
This command reads the current cluster state and saves it as json for 
human readability and editing.  Only state formats compatible with this
version of ipfs-cluster-service can be exported.  By default this command
prints the state to stdout.
`,
					Flags: []cli.Flag{
						cli.StringFlag{
							Name:  "file, f",
							Value: "",
							Usage: "sets an output file for exported state",
						},
					},
					Action: func(c *cli.Context) error {
						err := locker.lock()
						checkErr("acquiring execution lock", err)
						defer locker.tryUnlock()

						var w io.WriteCloser
						outputPath := c.String("file")
						if outputPath == "" {
							// Output to stdout
							w = os.Stdout
						} else {
							// Create the export file
							w, err = os.Create(outputPath)
							checkErr("creating output file", err)
						}
						defer w.Close()

						err = export(w)
						checkErr("exporting state", err)
						return nil
					},
				},
				{
					Name:  "import",
					Usage: "load an IPFS Cluster state from an exported state file",
					Description: `
This command reads in an exported state file storing the state as a persistent
snapshot to be loaded as the cluster state when the cluster peer is restarted.
If an argument is provided, cluster will treat it as the path of the file to
import.  If no argument is provided cluster will read json from stdin
`,
					Action: func(c *cli.Context) error {
						err := locker.lock()
						checkErr("acquiring execution lock", err)
						defer locker.tryUnlock()

						if !c.GlobalBool("force") {
							if !yesNoPrompt("The peer's state will be replaced.  Run with -h for details.  Continue? [y/n]:") {
								return nil
							}
						}

						// Get the importing file path
						importFile := c.Args().First()
						var r io.ReadCloser
						if importFile == "" {
							r = os.Stdin
							logger.Info("Reading from stdin, Ctrl-D to finish")
						} else {
							r, err = os.Open(importFile)
							checkErr("reading import file", err)
						}
						defer r.Close()
						err = stateImport(r)
						checkErr("importing state", err)
						logger.Info("the given state has been correctly imported to this peer.  Make sure all peers have consistent states")
						return nil
					},
				},
				{
					Name:  "cleanup",
					Usage: "cleanup persistent consensus state so cluster can start afresh",
					Description: `
This command removes the persistent state that is loaded on startup to determine this peer's view of the
cluster state.  While it removes the existing state from the load path, one invocation does not permanently remove
this state from disk.  This command renames cluster's data folder to <data-folder-name>.old.0, and rotates other
deprecated data folders to <data-folder-name>.old.<n+1>, etc for some rotation factor before permanatly deleting 
the mth data folder (m currently defaults to 5)
`,
					Action: func(c *cli.Context) error {
						err := locker.lock()
						checkErr("acquiring execution lock", err)
						defer locker.tryUnlock()

						if !c.GlobalBool("force") {
							if !yesNoPrompt("The peer's state will be removed from the load path.  Existing pins may be lost.  Continue? [y/n]:") {
								return nil
							}
						}

						cfgMgr, cfgs := makeConfigs()
						err = cfgMgr.LoadJSONFromFile(configPath)

						checkErr("initializing configs", err)

						dataFolder := filepath.Join(cfgs.consensusCfg.BaseDir, raft.DefaultDataSubFolder)
						err = raft.CleanupRaft(dataFolder)
						checkErr("Cleaning up consensus data", err)
						logger.Warningf("the %s folder has been rotated.  Next start will use an empty state", dataFolder)

						return nil
					},
				},
			},
		},
		{
			Name:  "version",
			Usage: "Print the ipfs-cluster version",
			Action: func(c *cli.Context) error {
				if c := ipfscluster.Commit; len(c) >= 8 {
					fmt.Printf("%s-%s\n", ipfscluster.Version, c)
					return nil
				}

				fmt.Printf("%s\n", ipfscluster.Version)
				return nil
			},
		},
	}

	app.Before = func(c *cli.Context) error {
		absPath, err := filepath.Abs(c.String("config"))
		if err != nil {
			return err
		}

		configPath = filepath.Join(absPath, DefaultConfigFile)

		setupLogLevel(c.String("loglevel"))
		if c.Bool("debug") {
			setupDebug()
		}

		locker = &lock{path: absPath}

		return nil
	}

	app.Action = run

	app.Run(os.Args)
}

// run daemon() by default, or error.
func run(c *cli.Context) error {
	if len(c.Args()) > 0 {
		return fmt.Errorf("unknown subcommand. Run \"%s help\" for more info", programName)
	}
	return daemon(c)
}

func daemon(c *cli.Context) error {
	logger.Info("Initializing. For verbose output run with \"-l debug\". Please wait...")

	// Load all the configurations
	cfgMgr, cfgs := makeConfigs()

	// Run any migrations
	if c.Bool("upgrade") {
		err := upgrade()
		if err != errNoSnapshot {
			checkErr("upgrading state", err)
		} // otherwise continue
	}

	// Execution lock
	err := locker.lock()
	checkErr("acquiring execution lock", err)
	defer locker.tryUnlock()

	// Load all the configurations
	// always wait for configuration to be saved
	defer cfgMgr.Shutdown()

	err = cfgMgr.LoadJSONFromFile(configPath)
	checkErr("loading configuration", err)

	if a := c.String("bootstrap"); a != "" {
		if len(cfgs.clusterCfg.Peers) > 0 && !c.Bool("force") {
			return errors.New("the configuration provides cluster.Peers. Use -f to ignore and proceed bootstrapping")
		}
		joinAddr, err := ma.NewMultiaddr(a)
		checkErr("error parsing multiaddress: %s", err)
		cfgs.clusterCfg.Bootstrap = []ma.Multiaddr{joinAddr}
		cfgs.clusterCfg.Peers = []ma.Multiaddr{}
	}

	if c.Bool("leave") {
		cfgs.clusterCfg.LeaveOnShutdown = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cluster, err := initializeCluster(ctx, c, cfgs)
	checkErr("starting cluster", err)

	signalChan := make(chan os.Signal, 20)
	signal.Notify(
		signalChan,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGHUP,
	)

	var ctrlcCount int
	for {
		select {
		case <-signalChan:
			ctrlcCount++
			handleCtrlC(cluster, ctrlcCount)
		case <-cluster.Done():
			return nil
		}
	}
}

func setupLogLevel(lvl string) {
	for f := range ipfscluster.LoggingFacilities {
		ipfscluster.SetFacilityLogLevel(f, lvl)
	}
}

func setupDebug() {
	ipfscluster.SetFacilityLogLevel("*", "DEBUG")
}

func setupAllocation(name string, diskInfCfg *disk.Config, numpinInfCfg *numpin.Config) (ipfscluster.Informer, ipfscluster.PinAllocator) {
	switch name {
	case "disk", "disk-freespace":
		informer, err := disk.NewInformer(diskInfCfg)
		checkErr("creating informer", err)
		return informer, descendalloc.NewAllocator()
	case "disk-reposize":
		informer, err := disk.NewInformer(diskInfCfg)
		checkErr("creating informer", err)
		return informer, ascendalloc.NewAllocator()
	case "numpin", "pincount":
		informer, err := numpin.NewInformer(numpinInfCfg)
		checkErr("creating informer", err)
		return informer, ascendalloc.NewAllocator()
	default:
		err := errors.New("unknown allocation strategy")
		checkErr("", err)
		return nil, nil
	}
}

func saveConfig(cfg *config.Manager, force bool) {
	if _, err := os.Stat(configPath); err == nil && !force {
		err := fmt.Errorf("%s exists. Try running: %s -f init", configPath, programName)
		checkErr("", err)
	}

	err := os.MkdirAll(filepath.Dir(configPath), 0700)
	err = cfg.SaveJSON(configPath)
	checkErr("saving new configuration", err)
	out("%s configuration written to %s\n",
		programName, configPath)
}

func userProvidedSecret(enterSecret bool) ([]byte, bool) {
	var secret string
	if enterSecret {
		secret = promptUser("Enter cluster secret (32-byte hex string): ")
	} else if envSecret, envSecretDefined := os.LookupEnv("CLUSTER_SECRET"); envSecretDefined {
		secret = envSecret
	} else {
		return nil, false
	}

	decodedSecret, err := ipfscluster.DecodeClusterSecret(secret)
	checkErr("parsing user-provided secret", err)
	return decodedSecret, true
}

func promptUser(msg string) string {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print(msg)
	scanner.Scan()
	return scanner.Text()
}

// Lifted from go-ipfs/cmd/ipfs/daemon.go
func yesNoPrompt(prompt string) bool {
	var s string
	for i := 0; i < 3; i++ {
		fmt.Printf("%s ", prompt)
		fmt.Scanf("%s", &s)
		switch s {
		case "y", "Y":
			return true
		case "n", "N":
			return false
		case "":
			return false
		}
		fmt.Println("Please press either 'y' or 'n'")
	}
	return false
}

func makeConfigs() (*config.Manager, *cfgs) {
	cfg := config.NewManager()
	clusterCfg := &ipfscluster.Config{}
	apiCfg := &rest.Config{}
	ipfshttpCfg := &ipfshttp.Config{}
	consensusCfg := &raft.Config{}
	trackerCfg := &maptracker.Config{}
	monCfg := &basic.Config{}
	diskInfCfg := &disk.Config{}
	numpinInfCfg := &numpin.Config{}
	sharderCfg := &sharder.Config{}
	cfg.RegisterComponent(config.Cluster, clusterCfg)
	cfg.RegisterComponent(config.API, apiCfg)
	cfg.RegisterComponent(config.IPFSConn, ipfshttpCfg)
	cfg.RegisterComponent(config.Consensus, consensusCfg)
	cfg.RegisterComponent(config.PinTracker, trackerCfg)
	cfg.RegisterComponent(config.Monitor, monCfg)
	cfg.RegisterComponent(config.Informer, diskInfCfg)
	cfg.RegisterComponent(config.Informer, numpinInfCfg)
	cfg.RegisterComponent(config.Sharder, sharderCfg)

	return cfg, &cfgs{clusterCfg, apiCfg, ipfshttpCfg, consensusCfg, trackerCfg, monCfg, diskInfCfg, numpinInfCfg, sharderCfg}
}

type cfgs struct {
	clusterCfg   *ipfscluster.Config
	apiCfg       *rest.Config
	ipfshttpCfg  *ipfshttp.Config
	consensusCfg *raft.Config
	trackerCfg   *maptracker.Config
	monCfg       *basic.Config
	diskInfCfg   *disk.Config
	numpinInfCfg *numpin.Config
	sharderCfg   *sharder.Config
}

func initializeCluster(ctx context.Context, c *cli.Context, cfgs *cfgs) (*ipfscluster.Cluster, error) {
	host, err := ipfscluster.NewClusterHost(ctx, cfgs.clusterCfg)
	checkErr("creating libP2P Host", err)

	api, err := rest.NewAPIWithHost(cfgs.apiCfg, host)
	checkErr("creating REST API component", err)

	proxy, err := ipfshttp.NewConnector(cfgs.ipfshttpCfg)
	checkErr("creating IPFS Connector component", err)

	state := mapstate.NewMapState()

	err = validateVersion(cfgs.clusterCfg, cfgs.consensusCfg)
	checkErr("validating version", err)

	tracker := maptracker.NewMapPinTracker(cfgs.trackerCfg, cfgs.clusterCfg.ID)
	mon, err := basic.NewMonitor(cfgs.monCfg)
	checkErr("creating Monitor component", err)
	informer, alloc := setupAllocation(c.GlobalString("alloc"), cfgs.diskInfCfg, cfgs.numpinInfCfg)

	sharder, err := sharder.NewSharder(cfgs.sharderCfg)
	checkErr("creating shard component", err)

	return ipfscluster.NewCluster(
		host,
		cfgs.clusterCfg,
		cfgs.consensusCfg,
		api,
		proxy,
		state,
		tracker,
		mon,
		alloc,
		informer,
		sharder,
	)
}

func handleCtrlC(cluster *ipfscluster.Cluster, ctrlcCount int) {
	switch ctrlcCount {
	case 1:
		go func() {
			err := cluster.Shutdown()
			checkErr("shutting down cluster", err)
		}()
	case 2:
		out(`


!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
Shutdown is taking too long! Press Ctrl-c again to manually kill cluster.
Note that this may corrupt the local cluster state.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!


`)
	case 3:
		out("exiting cluster NOW")
		os.Exit(-1)
	}
}
