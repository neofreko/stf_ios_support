package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"html/template"
	
	zmq "github.com/zeromq/goczmq"
	//ps "github.com/mitchellh/go-ps"
	ps "github.com/jviney/go-proc"
)

type Config struct {
	DeviceTrigger string `json:"device_trigger"`
	VideoEnabler string `json:"video_enabler"`
	SupportRoot string `json:"support_root"`
	MirrorFeedRoot string `json:"mirrorfeed_root"`
	WDARoot string `json:"wda_root"`
	CoordinatorHost string `json:"coordinator_host"`
	CoordinatorPort int `json:"coordinator_port"`
	WDAProxyPort string `json:"wda_proxy_port"`
	MirrorFeedPort int `json:"mirrorfeed_port"`
	Pipe string `json:"pipe"`
	SkipVideo bool `json:"skip_video"`
}

type DevEvent struct {
    action int
    uuid string
}

type PubEvent struct {
    action int
    uuid string
    name string
}

type RunningDev struct {
	uuid string
	name string
    mirror *os.Process
    ff     *os.Process
    proxy  *os.Process
}

type BaseProgs struct {
	trigger    *os.Process
	vidEnabler *os.Process
	stf        *os.Process
}

var gStop bool

func main() {
    gStop = false
    
    pubEventCh := make( chan PubEvent, 2 )
    
    if len( os.Args ) > 1 {
        arg := os.Args[1]
        fmt.Printf("option: %s\n", arg)
        
        if arg == "list" {
            //fmt.Printf("list\n");
            reqSock := zmq.NewSock( zmq.Req )
            defer reqSock.Destroy()
            
            err := reqSock.Connect("tcp://127.0.0.1:7293")
            if err != nil {
               log.Panicf("error binding: %s", err)
               os.Exit(1)
            }
                        
            reqMsg := []byte("request")
            reqSock.SendMessage([][]byte{reqMsg})
            
            reply, err := reqSock.RecvMessage()
            if err != nil {
               log.Panicf("error receiving: %s", err)
               os.Exit(1)
            }
            
            fmt.Printf("reply: %s\n", string( reply[0] ) )
        }
        if arg == "pull" {
            pullSock := zmq.NewSock( zmq.Sub )
            defer pullSock.Destroy()
            
            err := pullSock.Connect("tcp://127.0.0.1:7294")
            if err != nil {
               log.Panicf("error binding: %s", err)
               os.Exit(1)
            }
            
            for {
                jsonMsg, err := pullSock.RecvMessage()
                if err != nil {
                   log.Panicf("error receiving: %s", err)
                   os.Exit(1)
                }
                
                fmt.Printf("pulled: %s\n", string( jsonMsg[0] ) )
                
                //var msg DevEvent
                //json.Unmarshal( jsonMsg[0], &msg )
            }
        }
        if arg == "server" {
            zmqReqRep()
            zmqPub( pubEventCh )
            var num int = 1
            for {
                devEvent := PubEvent{}
                devEvent.action = 0 // connect
                devEvent.name = "test"
                uuid := fmt.Sprintf("fakeuuid %d", num)
                devEvent.uuid = uuid
                num++
                pubEventCh <- devEvent
                
                time.Sleep( time.Second * 5 )
            }
        }
        
        return
    }
    
    zmqReqRep()
    zmqPub( pubEventCh )
    
	// Read in config
	configFile := "config.json"
	configFh, err := os.Open(configFile)   
    if err != nil {
        log.Panicf("failed reading file: %s", err)
    }
    defer configFh.Close()
    
	jsonBytes, _ := ioutil.ReadAll( configFh )
	var config Config
	json.Unmarshal( jsonBytes, &config )
	
	var vpnMissing bool = true
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
	    //fmt.Printf( "iface %v\n", iface.Name )
	    if iface.Name == "utun1" {
	        vpnMissing = false
	    }
	}
	//os.Exit(0)
	
	// Cleanup hanging processes if any
    procs := ps.GetAllProcessesInfo()
    for _, proc := range procs {
        cmd := proc.CommandLine
        //fmt.Printf("Proc: pid=%d %s\n", proc.Pid, proc.CommandLine )
        cmdFlat := strings.Join( cmd, " " )
        if cmdFlat == "/bin/bash run-stf.sh" {
            fmt.Printf("Leftover STF - Sending SIGTERM\n")
            syscall.Kill( proc.Pid, syscall.SIGTERM )
        }
        if cmdFlat == config.VideoEnabler {
            fmt.Printf("Leftover Video enabler - Sending SIGTERM\n")
            syscall.Kill( proc.Pid, syscall.SIGTERM )
        }
        if cmdFlat == config.DeviceTrigger {
            fmt.Printf("Leftover Device trigger - Sending SIGTERM\n")
            syscall.Kill( proc.Pid, syscall.SIGTERM )
        }
        if cmd[0] == "node" && cmd[1] == "--inspect=127.0.0.1:9230" {
            fmt.Printf("Leftover STF(via node) - Sending SIGTERM\n")
            syscall.Kill( proc.Pid, syscall.SIGTERM )
        }
    }
    
	devEventCh := make( chan DevEvent, 2 )
	runningDevs := make( map [string] RunningDev )
	baseProgs := BaseProgs{}
	
	// start web server waiting for trigger http command for device connect and disconnect
	
	var listen_addr = fmt.Sprintf( "%s:%d", config.CoordinatorHost, config.CoordinatorPort ) // "localhost:8027"
	go startServer( devEventCh, listen_addr )
	
	// start the 'osx_ios_device_trigger'
	go func() {
		fmt.Printf("Starting osx_ios_device_trigger\n");
		triggerCmd := exec.Command( config.DeviceTrigger )
		
		//triggerOut, _ := triggerCmd.StdoutPipe()
		//triggerCmd.Stdout = os.Stdout
		//triggerCmd.Stderr = os.Stderr
		err := triggerCmd.Start()
		if err != nil {
			fmt.Println(err.Error())
		} else {
			baseProgs.trigger = triggerCmd.Process
		}
		/*for {
			line, err := ioutil.Read(triggerOut)
			if err != nil {
				break
			}
		}*/
		triggerCmd.Wait()
		fmt.Printf("Ended: osx_ios_device_trigger\n");
	}()
	
	// start the video enabler
	go func() {
		fmt.Printf("Starting video-enabler\n");
		enableCmd := exec.Command(config.VideoEnabler)
		err := enableCmd.Start()
		if err != nil {
			fmt.Println(err.Error())
			baseProgs.vidEnabler = nil
		} else {
			baseProgs.vidEnabler = enableCmd.Process 
		}
		enableCmd.Wait()
		fmt.Printf("Ended: video-enabler\n")
	}()
	
	if vpnMissing {
	    fmt.Printf("VPN not enabled; skipping start of STF\n")
	    baseProgs.stf = nil
	} else {
        // start stf and restart it when needed
        // TODO: if it doesn't restart / crashes again; give up
        go func() {
            for {
                fmt.Printf("Starting stf\n");
                stfCmd := exec.Command("/bin/bash", "run-stf.sh")
                stfCmd.Stdout = os.Stdout
                stfCmd.Stderr = os.Stderr
                
                err := stfCmd.Start()
                if err != nil {
                    fmt.Println(err.Error())
                    baseProgs.stf = nil
                } else {
                    baseProgs.stf = stfCmd.Process
                }
                stfCmd.Wait()
                fmt.Printf("Ended:stf\n");
                // log out that it stopped
            }
        }()
    }
	
	SetupCloseHandler( runningDevs, &baseProgs )
	
	/*go func() {
		// repeatedly check vpn connection
				
		// when vpn goes down
			// log an error
			// wait for it to come back up
			// restart the various things to use the new IP
	}*/

    for {
        // receive message
        devEvent := <- devEventCh
        uuid := devEvent.uuid
        
        if devEvent.action == 0 { // device connect
            devd := RunningDev{}
            devd.uuid = uuid
            fmt.Printf("Setting up device uuid: %s\n", uuid)
            devd.name = getDeviceName( uuid )
            devName := devd.name
            fmt.Printf("Device name: %s\n", devName)
            
            if config.SkipVideo {
                devd.mirror = nil
                devd.ff = nil
            } else {
                // start mirrorfeed
                mirrorPort := config.MirrorFeedPort // 8000
                pipeName := config.Pipe
                fmt.Printf("Starting mirrorfeed\n");
                
                mirrorFeedBin := fmt.Sprintf( "%s/mirrorfeed/mirrorfeed", config.MirrorFeedRoot )
                
                mirrorCmd := exec.Command(mirrorFeedBin, strconv.Itoa( mirrorPort ), pipeName )
                mirrorCmd.Stdout = os.Stdout
                mirrorCmd.Stderr = os.Stderr
                go func() {
                    err := mirrorCmd.Start()
                    if err != nil {
                        fmt.Println(err.Error())
                        devd.mirror = nil
                    } else {
                        devd.mirror = mirrorCmd.Process
                    }
                    mirrorCmd.Wait()
                    fmt.Printf("mirrorfeed ended\n")
                    devd.mirror = nil
                }()
            
                halfresScript := fmt.Sprintf( "%s/mirrorfeed/halfres.sh", config.MirrorFeedRoot )
            
                // start ffmpeg
                fmt.Printf("Starting ffmpeg\n")
                fmt.Printf("  /bin/bash %s \"%s\" %s\n", halfresScript, devName, pipeName )
            
                ffCmd := exec.Command("/bin/bash", halfresScript, devName, pipeName )
                //ffCmd.Stdout = os.Stdout
                //ffCmd.Stderr = os.Stderr
                go func() {
                    err := ffCmd.Start()
                    if err != nil {
                        fmt.Println(err.Error())
                        devd.ff = nil
                    } else {
                        devd.ff = ffCmd.Process
                    }
                    ffCmd.Wait()
                    fmt.Printf("ffmpeg ended\n")
                    devd.ff = nil
                }()
            
                // Sleep to ensure that video enabling process is finished before we try to start wdaproxy
                // This is needed because the USB device drops out and reappears during video enabling
                time.Sleep( time.Second * 9 )
            }
            
            // start wdaproxy
            wdaPort := config.WDAProxyPort // "8100"
            fmt.Printf("Starting wdaproxy\n")
            fmt.Printf("  wdaproxy -p %s -d -W %s -u %s\n", wdaPort, config.WDARoot, uuid )
            proxyCmd := exec.Command( "wdaproxy", "-p", wdaPort, "-d", "-W", config.WDARoot, "-u", uuid )
            proxyCmd.Stdout = os.Stdout
            proxyCmd.Stderr = os.Stderr
            go func() {
                err := proxyCmd.Start()
                if err != nil {
                    fmt.Println(err.Error())
                    devd.proxy = nil
                } else {
                    devd.proxy = proxyCmd.Process
                }
                
                time.Sleep( time.Second * 3 )
                
                // Everything is started; notify stf via zmq published event
                pubEvent := PubEvent{}
                pubEvent.action = devEvent.action
                pubEvent.uuid = devEvent.uuid
                pubEvent.name = devName
                pubEventCh <- pubEvent
                
                proxyCmd.Wait()
                fmt.Printf("wdaproxy ended\n")
            }()
            
            runningDevs[uuid] = devd
        }
        if devEvent.action == 1 { // device disconnect
            devd := runningDevs[uuid]
            closeRunningDev( devd )
            
            // Notify stf that the device is gone
            pubEvent := PubEvent{}
            pubEvent.action = devEvent.action
            pubEvent.uuid = devEvent.uuid
            pubEvent.name = ""
            pubEventCh <- pubEvent
        }
    }
}

func zmqPub( pubEventCh <-chan PubEvent ) {
    var sentDummy bool = false
    
    // start the zmq pub mechanism
	go func() {
	    pubSock := zmq.NewSock(zmq.Pub)
	    //pubSock, _ := zmq.NewPub("tcp://127.0.0.1:7294/x")
        defer pubSock.Destroy()
        
        _, err := pubSock.Bind("tcp://127.0.0.1:7294")
        if err != nil {
           log.Panicf("error binding: %s", err)
           os.Exit(1)
        }
        
        // Garbage message with delay to avoid late joiner ZeroMQ madness
        if !sentDummy {
            pubSock.SendMessage( [][]byte{ []byte("devEvent"), []byte("dummy") } )
            time.Sleep( time.Millisecond * 300 )
        }
        
        for {
            // receive message
            pubEvent := <- pubEventCh
            
            //uuid := devEvent.uuid
            type DevTest struct {
                Type string
                UUID string
                Name string
            }
            test := DevTest{}
            test.UUID = pubEvent.uuid
            test.Name = pubEvent.name
            
            if pubEvent.action == 0 {
                test.Type = "connect"
            } else {
                test.Type = "disconnect"
            }
            
            // publish a zmq message of the DevEvent
            reqMsg, err := json.Marshal( test )
            if err != nil {
               log.Panicf("error encoding JSON: %s", err)
            }
            fmt.Printf("Publishing to stf: %s\n", reqMsg )
            
            /*err = pubSock.SendFrame([]byte(reqMsg), zmq.FlagNone )
            if err != nil {
               log.Panicf("error encoding JSON: %s", err)
            }*/
            pubSock.SendMessage( [][]byte{ []byte("devEvent"), reqMsg} )
        }
	}()
}

func zmqReqRep() {
    go func() {
        repSock := zmq.NewSock(zmq.Rep)
        defer repSock.Destroy()
        
        _, err := repSock.Bind("tcp://127.0.0.1:7293")
        if err != nil {
           log.Panicf("error binding: %s", err)
           os.Exit(1)
        }
        
        repOb, err := zmq.NewReadWriter(repSock)
        if err != nil {
           log.Panicf("error making readwriter: %s", err)
           os.Exit(1)
        }
        defer repOb.Destroy()
        
        repOb.SetTimeout(1000)
        
        for {
            /*msg, err := repSock.RecvMessage()
            if err != nil {
               log.Panicf("error receiving: %s", err)
               os.Exit(1)
            }
            
            fmt.Printf("Received: %s\n", string( msg[0] ) )
            
            response := []byte("response")
            repSock.SendMessage([][]byte{response})*/
            
            buf := make([]byte, 2000)
            _, err := repOb.Read( buf )
            if err == zmq.ErrTimeout {
                if gStop == true {
                    break
                }
                continue
            }
            if err != nil && err != io.EOF {
               log.Panicf("error receiving: %s", err)
               os.Exit(1)
            }
            msg := string( buf )
            
            if msg == "quit" {
                response := []byte("quitting")
                repSock.SendMessage([][]byte{response})
                break
            } else if msg == "devices" {
                // TODO: get device list
                // TOOO: turn device list into JSON
                
                response := []byte("quitting")
                repSock.SendMessage([][]byte{response})
            } else {
                fmt.Printf("Received: %s\n", string( buf ) )
            
                response := []byte("response")
                repSock.SendMessage([][]byte{response})
            }
        }
    }()
}

func closeAllRunningDevs( runningDevs map [string] RunningDev ) {
	for _, devd := range runningDevs {
		closeRunningDev( devd )
	}
}

func closeRunningDev( devd RunningDev ) {
	// stop wdaproxy
	if devd.proxy != nil {
		fmt.Printf("Killing wdaproxy\n")
		devd.proxy.Kill()
	}
	
	// stop ffmpeg
	if devd.ff != nil {
		fmt.Printf("Killing ffmpeg\n")
		devd.ff.Kill()
	}
	
	// stop mirrorfeed
	if devd.mirror != nil {
		fmt.Printf("Killing mirrorfeed\n")
		devd.mirror.Kill()
	}
}

func closeBaseProgs( baseProgs *BaseProgs ) {
	if baseProgs.trigger != nil {
		fmt.Printf("Killing trigger\n")
		baseProgs.trigger.Kill()
	}
	if baseProgs.vidEnabler != nil {
		fmt.Printf("Killing vidEnabler\n")
		baseProgs.vidEnabler.Kill()
	}
	if baseProgs.stf != nil {
		fmt.Printf("Killing stf\n")
		baseProgs.stf.Kill()
	}
}

func SetupCloseHandler( runningDevs map [string] RunningDev, baseProgs *BaseProgs ) {
    c := make(chan os.Signal, 2)
    signal.Notify(c, os.Interrupt, syscall.SIGTERM)
    go func() {
        <- c
        fmt.Println("\r- Ctrl+C pressed in Terminal")
        closeBaseProgs( baseProgs )
        closeAllRunningDevs( runningDevs )
        
        // This triggers zmq to stop receiving
        // We don't actually wait after this to ensure it has finished cleanly... oh well :)
        gStop = true
        
        os.Exit(0)
    }()
}

func getDeviceName( uuid string ) (string) {
	name, _ := exec.Command( "idevicename", "-u", uuid ).Output()
	if name == nil {
	    fmt.Printf("idevicename returned nothing for uuid %s\n", uuid)
	}
	nameStr := string(name)
	nameStr = nameStr[:len(nameStr)-1]
	return nameStr
}
	
func startServer( devEventCh chan<- DevEvent, listen_addr string ) {
    fmt.Printf("Starting server\n");
    http.HandleFunc( "/", handleRoot )
    connectClosure := func( w http.ResponseWriter, r *http.Request ) {
    	deviceConnect( w, r, devEventCh )
    }
    disconnectClosure := func( w http.ResponseWriter, r *http.Request ) {
    	deviceDisconnect( w, r, devEventCh )
    }
    http.HandleFunc( "/dev_connect", connectClosure )
    http.HandleFunc( "/dev_disconnect", disconnectClosure )
    log.Fatal( http.ListenAndServe( listen_addr, nil ) )
}

func handleRoot( w http.ResponseWriter, r *http.Request ) {
    rootTpl.Execute( w, "ws://"+r.Host+"/echo" )
}

func deviceConnect( w http.ResponseWriter, r *http.Request, devEventCh chan<- DevEvent ) {
	// signal device loop of device connect
	devEvent := DevEvent{}
	devEvent.action = 0
	r.ParseForm()
	devEvent.uuid = r.Form.Get("uuid")
	devEventCh <- devEvent
}

func deviceDisconnect( w http.ResponseWriter, r *http.Request, devEventCh chan<- DevEvent ) {
	// signal device loop of device disconnect
	devEvent := DevEvent{}
	devEvent.action = 1
	r.ParseForm()
	devEvent.uuid = r.Form.Get("uuid")
	devEventCh <- devEvent
}

var rootTpl = template.Must(template.New("").Parse(`
<!DOCTYPE html>
<html>
	<head>
	</head>
	<body>
	test
	</body>
</html>
`))