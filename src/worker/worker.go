package worker

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	dutil "github.com/open-lambda/open-lambda/ol/worker/sandbox/dockerutil"

	"github.com/open-lambda/open-lambda/ol/common"
	"github.com/open-lambda/open-lambda/ol/worker/server"

	"github.com/urfave/cli"	
)

// modify the config.json file based on settings from cmdline: -o opt1=val1,opt2=val2,...
//
// apply changes in optsStr to config from confPath, saving result to overridePath
func overrideOpts(confPath, overridePath, optsStr string) error {
	b, err := ioutil.ReadFile(confPath)
	if err != nil {
		return err
	}
	conf := make(map[string]any)
	if err := json.Unmarshal(b, &conf); err != nil {
		return err
	}

	opts := strings.Split(optsStr, ",")
	for _, opt := range opts {
		parts := strings.Split(opt, "=")
		if len(parts) != 2 {
			return fmt.Errorf("Could not parse key=val: '%s'", opt)
		}
		keys := strings.Split(parts[0], ".")
		val := parts[1]

		c := conf
		for i := 0; i < len(keys)-1; i++ {
			sub, ok := c[keys[i]]
			if !ok {
				return fmt.Errorf("key '%s' not found", keys[i])
			}
			switch v := sub.(type) {
			case map[string]any:
				c = v
			default:
				return fmt.Errorf("%s refers to a %T, not a map", keys[i], c[keys[i]])
			}
		}

		key := keys[len(keys)-1]
		prev, ok := c[key]
		if !ok {
			return fmt.Errorf("invalid option: '%s'", key)
		}
		switch prev.(type) {
		case string:
			c[key] = val
		case float64:
			c[key], err = strconv.Atoi(val)
			if err != nil {
				return err
			}
		case bool:
			if strings.ToLower(val) == "true" {
				c[key] = true
			} else if strings.ToLower(val) == "false" {
				c[key] = false
			} else {
				return fmt.Errorf("'%s' for %s not a valid boolean value", val, key)
			}
		default:
			return fmt.Errorf("config values of type %T (%s) must be edited manually in the config file ", prev, key)
		}
	}

	// save back config
	s, err := json.MarshalIndent(conf, "", "\t")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(overridePath, s, 0644); err != nil {
		return err
	}
	return nil
}

func initOLDir(olPath string, dockerBaseImage string) (err error) {
	if dockerBaseImage == "" {
		dockerBaseImage = "ol-wasm"
	}

	fmt.Printf("Init OL dir at %v, using Docker image %v as base\n", olPath, dockerBaseImage)
	if err := os.Mkdir(olPath, 0700); err != nil {
		return err
	}

	if err := common.LoadDefaults(olPath); err != nil {
		return err
	}

	confPath := filepath.Join(olPath, "config.json")
	if err := common.SaveConf(confPath); err != nil {
		return err
	}

	if err := os.Mkdir(common.Conf.Worker_dir, 0700); err != nil {
		return err
	}

	if err := os.Mkdir(common.Conf.Registry, 0700); err != nil {
		return err
	}

	// create a base directory to run sock handlers
	var dockerClient *docker.Client
	if c, err := docker.NewClientFromEnv(); err != nil {
		return err
	} else {
		dockerClient = c
	}

	base := common.Conf.SOCK_base_path
	fmt.Printf("Creating lambda base at %v (may take several minutes)\n", base)
	err = dutil.DumpDockerImage(dockerClient, dockerBaseImage, base)
	if err != nil {
		return err
	}

	if err := os.Mkdir(path.Join(base, "handler"), 0700); err != nil {
		return err
	}

	if err := os.Mkdir(path.Join(base, "host"), 0700); err != nil {
		return err
	}

	if err := os.Mkdir(path.Join(base, "packages"), 0700); err != nil {
		return err
	}

	// need this because Docker containers don't have a dns server in /etc/resolv.conf
	dnsPath := filepath.Join(base, "etc", "resolv.conf")
	if err := ioutil.WriteFile(dnsPath, []byte("nameserver 8.8.8.8\n"), 0644); err != nil {
		return err
	}

	path := filepath.Join(base, "dev", "null")
	if err := exec.Command("mknod", "-m", "0644", path, "c", "1", "3").Run(); err != nil {
		return err
	}

	path = filepath.Join(base, "dev", "random")
	if err := exec.Command("mknod", "-m", "0644", path, "c", "1", "8").Run(); err != nil {
		return err
	}

	path = filepath.Join(base, "dev", "urandom")
	if err := exec.Command("mknod", "-m", "0644", path, "c", "1", "9").Run(); err != nil {
		return err
	}

	fmt.Printf("Working Directory: %s\n\n", olPath)
	fmt.Printf("Worker Defaults: \n%s\n\n", common.DumpConfStr())
	fmt.Printf("You may modify the defaults here: %s\n\n", confPath)
	fmt.Printf("You may now start a server using the \"ol worker\" command\n")

	return nil
}

// newOL corresponds to the "new" command of the admin tool.
func newOL(ctx *cli.Context) error {
	olPath, err := common.GetOlPath(ctx)
	if err != nil {
		return err
	}

	return initOLDir(olPath, ctx.String("image"))
}

// status corresponds to the "status" command of the admin tool.
func status(ctx *cli.Context) error {
	olPath, err := common.GetOlPath(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Worker Ping:\n")
	err = common.LoadConf(filepath.Join(olPath, "config.json"))
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://localhost:%s/status", common.Conf.Worker_port)
	response, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("could not send GET to %s", url)
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("failed to read body from GET to %s", url)
	}
	fmt.Printf("  %s => %s [%s]\n", url, body, response.Status)
	fmt.Printf("\n")

	return nil
}

// "up" corresponds to the "up" command of the admin tool.
//
// The JSON config in the cluster template directory will be populated for each
// worker, and their pid will be written to the log directory. worker_exec will
// be called to run the worker processes.
func up(ctx *cli.Context) error {
	// get path of worker files
	olPath, err := common.GetOlPath(ctx)
	if err != nil {
		return err
	}

	// if `./ol new` not previously run, do that init now
	if _, err := os.Stat(olPath); os.IsNotExist(err) {
		fmt.Printf("No OL directory found at %s\n", olPath)
		if err := initOLDir(olPath, ctx.String("image")); err != nil {
			return err
		}
	} else {
		fmt.Printf("using existing OL directory at %s\n", olPath)
	}

	confPath := filepath.Join(olPath, "config.json")
	overrides := ctx.String("options")
	if overrides != "" {
		overridesPath := confPath + ".overrides"
		err = overrideOpts(confPath, overridesPath, overrides)
		if err != nil {
			return err
		}
		confPath = overridesPath
	}

	if err := common.LoadConf(confPath); err != nil {
		return err
	}

	// should we run as a background process?
	detach := ctx.Bool("detach")

	if detach {
		// stdout+stderr both go to log
		logPath := filepath.Join(olPath, "worker.out")
		// creates a worker.out file
		f, err := os.Create(logPath)
		if err != nil {
			return err
		}
		// holds attributes that will be used when os.StartProcess.
		// we use CLONE_NEWNS because ol creates many mount points.
		// we don't want them to show up in /proc/self/mountinfo
		// for systemd because systemd creates a service for each
		// mount point, which is a major overhead.
		attr := os.ProcAttr{
			Files: []*os.File{nil, f, f},
			Sys: &syscall.SysProcAttr{
				Unshareflags: syscall.CLONE_NEWNS,
			},
		}
		cmd := []string{}
		for _, arg := range os.Args {
			if arg != "-d" && arg != "--detach" {
				cmd = append(cmd, arg)
			}
		}
		// looks for ./ol path
		binPath, err := exec.LookPath(os.Args[0])
		if err != nil {
			return err
		}
		// start the worker process
		fmt.Printf("starting process: binpath= %s, cmd=%s\n", binPath, cmd)
		proc, err := os.StartProcess(binPath, cmd, &attr)
		if err != nil {
			return err
		}

		// died is error message
		died := make(chan error)
		go func() {
			_, err := proc.Wait()
			died <- err
		}()

		fmt.Printf("Starting worker: pid=%d, port=%s, log=%s\n", proc.Pid, common.Conf.Worker_port, logPath)

		var pingErr error

		for i := 0; i < 300; i++ {
			// check if it has died
			select {
			case err := <-died:
				if err != nil {
					return err
				}
				return fmt.Errorf("worker process %d does not a appear to be running, check worker.out", proc.Pid)
			default:
			}

			// is the worker still alive?
			err := proc.Signal(syscall.Signal(0))
			if err != nil {

			}

			// is it reachable?
			url := fmt.Sprintf("http://localhost:%s/pid", common.Conf.Worker_port)
			response, err := http.Get(url)
			if err != nil {
				pingErr = err
				time.Sleep(100 * time.Millisecond)
				continue
			}
			defer response.Body.Close()

			// are we talking with the expected PID?
			body, err := ioutil.ReadAll(response.Body)
			pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
			if err != nil {
				return fmt.Errorf("/pid did not return an int :: %s", err)
			}

			if pid == proc.Pid {
				fmt.Printf("ready\n")
				return nil // server is started and ready for requests
			}

			return fmt.Errorf("expected PID %v but found %v (port conflict?)", proc.Pid, pid)
		}

		return fmt.Errorf("worker still not reachable after 30 seconds :: %s", pingErr)
	}

	if err := server.Main(); err != nil {
		return err
	}

	return fmt.Errorf("this code should not be reachable")
}

// down corresponds to the "down" command of the admin tool.
func down(ctx *cli.Context) error {
	olPath, err := common.GetOlPath(ctx)
	if err != nil {
		return err
	}

	// locate worker.pid, use it to get worker's PID
	configPath := filepath.Join(olPath, "config.json")
	if err := common.LoadConf(configPath); err != nil {
		return err
	}
	data, err := ioutil.ReadFile(filepath.Join(common.Conf.Worker_dir, "worker.pid"))
	if err != nil {
		return err
	}
	pidstr := string(data)
	pid, err := strconv.Atoi(pidstr)
	if err != nil {
		return err
	}

	fmt.Printf("Killing worker process with PID %d\n", pid)
	p, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		fmt.Printf("Failed to find worker process with PID %d.  May require manual cleanup.\n", pid)
	}
	if err := p.Signal(syscall.SIGINT); err != nil {
		fmt.Printf("%s\n", err.Error())
		fmt.Printf("Failed to kill process with PID %d.  May require manual cleanup.\n", pid)
	}

	for i := 0; i < 300; i++ {
		err := p.Signal(syscall.Signal(0))
		if err != nil {
			fmt.Printf("OL worker process stopped successfully\n")
			return nil // good, process must have stopped
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("worker didn't stop after 30s")
}

// cleanup corresponds to the "force-cleanup" command of the admin tool.
func cleanup(ctx *cli.Context) error {
	olPath, err := common.GetOlPath(ctx)
	if err != nil {
		return err
	}

	cgRoot := filepath.Join("/sys", "fs", "cgroup", filepath.Base(olPath)+"-sandboxes")
	fmt.Printf("ATTEMPT to cleanup cgroups at %s\n", cgRoot)

	if files, err := ioutil.ReadDir(cgRoot); err != nil {
		fmt.Printf("could not find cgroup root: %s\n", err.Error())
	} else {
		kill := filepath.Join(cgRoot, "cgroup.kill")
		if err := ioutil.WriteFile(kill, []byte(fmt.Sprintf("%d", 1)), os.ModeAppend); err != nil {
			fmt.Printf("could kill processes in cgroup: %s\n", err.Error())
		}

		for _, file := range files {
			if strings.HasPrefix(file.Name(), "cg-") {
				cg := filepath.Join(cgRoot, file.Name())
				fmt.Printf("try removing %s\n", cg)
				if err := syscall.Rmdir(cg); err != nil {
					fmt.Printf("could remove cgroup: %s\n", err.Error())
				}
			}
		}

		if err := syscall.Rmdir(cgRoot); err != nil {
			fmt.Printf("could remove cgroup root: %s\n", err.Error())
		}
	}

	dirName := filepath.Join(olPath, "worker", "root-sandboxes")
	fmt.Printf("ATTEMPT to cleanup mounts at %s\n", dirName)

	if files, err := ioutil.ReadDir(dirName); err != nil {
		fmt.Printf("could not find mount root: %s\n", err.Error())
	} else {
		for _, file := range files {
			path := filepath.Join(dirName, file.Name())
			fmt.Printf("try unmounting %s\n", path)
			if err := syscall.Unmount(path, syscall.MNT_DETACH); err != nil {
				fmt.Printf("could not unmount: %s\n", err.Error())
			}

			if err := syscall.Rmdir(path); err != nil {
				fmt.Printf("could remove mount dir: %s\n", err.Error())
			}
		}
	}

	if err := syscall.Unmount(dirName, syscall.MNT_DETACH); err != nil {
		fmt.Printf("could not unmount %s: %s\n", dirName, err.Error())
	}

	if err := os.Remove(filepath.Join(olPath, "worker", "worker.pid")); err != nil {
		fmt.Printf("could not remove worker.pid: %s\n", err.Error())
	}

	return nil
}

func WorkerCommands() []cli.Command {
	pathFlag := cli.StringFlag{
		Name:  "path, p",
		Usage: "Path location for OL environment",
	}
	dockerImgFlag := cli.StringFlag{
		Name:  "image, i",
		Usage: "Name of Docker image to use for base",
	}

	cmds := []cli.Command{
		cli.Command{
			Name:        "new",
			Usage:       "Create an OL worker environment, including default config and dump of base image",
			UsageText:   "ol new [--path=PATH] [--image=DOCKER-IMAGE]",
			Description: "A cluster directory of the given name will be created with internal structure initialized.",
			Flags:       []cli.Flag{pathFlag, dockerImgFlag},
			Action:      newOL,
		},
		cli.Command{
			Name:        "up",
			Usage:       "Start an OL worker process (automatically calls 'new' and uses default if that wasn't already done)",
			UsageText:   "ol up [--path=NAME] [--image=DOCKER-IMAGE] [--detach]",
			Description: "Start an OL worker.",
			Flags: []cli.Flag{
				pathFlag,
				dockerImgFlag,
				cli.StringFlag{
					Name:  "options, o",
					Usage: "Override options with: -o opt1=val1,opt2=val2/opt3.subopt31=val3",
				},
				cli.BoolFlag{
					Name:  "detach, d",
					Usage: "Run worker in background",
				},
			},
			Action: up,
		},
		cli.Command{
			Name:      "down",
			Usage:     "Kill containers and processes of the worker",
			UsageText: "ol down [--path=NAME]",
			Flags:     []cli.Flag{pathFlag},
			Action:    down,
		},
		cli.Command{
			Name:        "status",
			Usage:       "check status of an OL worker process",
			UsageText:   "ol status [--path=NAME]",
			Description: "If no cluster name is specified, number of containers of each cluster is printed; otherwise the connection information for all containers in the given cluster will be displayed.",
			Flags:       []cli.Flag{pathFlag},
			Action:      status,
		},
		cli.Command{
			Name:      "force-cleanup",
			Usage:     "Developer use only.  Cleanup cgroups and mount points (only needed when OL halted unexpectedly or there's a bug)",
			UsageText: "ol force-cleanup [--path=NAME]",
			Flags:     []cli.Flag{pathFlag},
			Action:    cleanup,
		},
	}

	return cmds
}