package main

import (
	"encoding/json"
	"fmt"
	"github.com/mitchellh/go-ps"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	storeDir      = "/root/logs/"
	processName   = "java"
	prometheusSvc = "http://promtheus-0.promtheus-headless.thanos.svc.cluster.local:9090"
	cpuRateMetric = "container_cpu_usage_rate"
)

type queryResp struct {
	Status string   `json:"status"`
	Data   respData `json:"data"`
}

type respData struct {
	ResultType string       `json:"resultType"`
	Result     []dataResult `json:"result"`
}

type dataResult struct {
	Metric interface{}   `json:"metric"`
	Value  []interface{} `json:"value"`
}

type cliArg struct {
	podName               string
	cpuLimit              int
	alertThreshold        int
	alertCpuAvgRate       float64
	assessTotalSeconds    int
	assessSilentSeconds   int
	assessIntervalSeconds int
}

func main() {
	dumpSignalChan := make(chan bool)
	defaultArg := cliArg{
		podName:               "tuia-algo-engine-normal-prd-8484967c75-b58m5",
		alertThreshold:        5,
		alertCpuAvgRate:       35,
		assessTotalSeconds:    120,
		assessIntervalSeconds: 15,
		assessSilentSeconds:   120,
		cpuLimit:              8,
	}
	go func() {
		hits := 0
		assessTimestamp := time.Now().UnixNano()
		silent := false
		var currentRate float64
		var timeGap time.Duration
		for {
			if silent {
				log.Printf("keep silent for about %d seconds\n", defaultArg.assessSilentSeconds)
				time.Sleep(time.Duration(defaultArg.assessSilentSeconds) * time.Second)
				log.Println("exit silent and start assess again")
				silent = false
				hits = 0
				assessTimestamp = time.Now().UnixNano()
			} else {
				currentRate = fetchCurrentCpuAvgRate(defaultArg)
				timeGap = time.Duration(time.Now().UnixNano() - assessTimestamp)
				if timeGap < time.Duration(defaultArg.assessTotalSeconds)*time.Second {
					// 评估时段内，允许非连续性的条件满足，从而避免间歇性的突刺高峰被忽略。
					// 比如在 5 分钟内每隔 30s 评估一次，评估满足的阈值为 5。
					// 则：可以第一个30s条件满足，第二个30s条件不满足，只要在 5 分钟内总计有 5 次条件满足，就算整个评估满足了
					if currentRate > defaultArg.alertCpuAvgRate {
						hits++
					}
					log.Printf("rate(current/alert) -> %f/%f, hits(current/alert) -> %d/%d\n", currentRate, defaultArg.alertCpuAvgRate, hits, defaultArg.alertThreshold)
				} else {
					log.Println("exceed assess total seconds, reset")
					hits = 0
					assessTimestamp = time.Now().UnixNano()
				}
				if hits >= defaultArg.alertThreshold {
					dumpSignalChan <- true
					silent = true
				} else {
					time.Sleep(time.Duration(defaultArg.assessIntervalSeconds) * time.Second)
				}
			}
		}
	}()
	for {
		select {
		case <-dumpSignalChan:
			doThreadDump(defaultArg.podName, findJavaPid())
		default:
			time.Sleep(5 * time.Second)
		}
	}
}

// 从 prometheus 处取得 cpu 平均使用率
func fetchCurrentCpuAvgRate(arg cliArg) float64 {
	apiUrl := prometheusSvc + "/api/v1/query?query=" + buildQueryParam(arg.podName, arg.cpuLimit)
	resp, err := http.Get(apiUrl)
	if err != nil || resp.StatusCode != 200 {
		log.Printf("response failed with status code: %d \n", resp.StatusCode)
		return -1
	}
	defer resp.Body.Close()
	respBody := queryResp{}
	err = json.NewDecoder(resp.Body).Decode(&respBody)
	if err != nil {
		log.Println("convert resp body to json failed, reason -> ", err)
	}
	valueStr := fmt.Sprintf("%v", respBody.Data.Result[0].Value[1])
	value, _ := strconv.ParseFloat(valueStr, 64)
	return value
}

func buildQueryParam(podName string, cpuLimit int) string {
	return cpuRateMetric + "{pod=\"" + podName + "\"}/" + strconv.Itoa(cpuLimit) + "*100"
}

// 由于开了 shareProcessNamespace，所以业务容器的进程 id 不是固定的 1 了，得透过 match 的方式取得
func findJavaPid() int {
	processes, err := ps.Processes()
	if err != nil {
		log.Println("get processes failed, reason -> ", err)
	}
	for _, p := range processes {
		if strings.Contains(p.Executable(), processName) {
			return p.Pid()
		}
	}
	return -1
}

// 执行线程 dump，用 jstack
func doThreadDump(podName string, pid int) {
	log.Printf("start thread dump, pid: %d\n", pid)
	cmd := exec.Command("jstack", strconv.Itoa(pid))
	output, err := cmd.Output()
	if err != nil {
		log.Println("failed to do thread dump, reason -> ", err)
	}
	now := time.Now().Format(time.RFC3339)
	err = os.WriteFile(storeDir+podName+"-"+now+"-jstack.txt", output, 0644)
	if err != nil {
		log.Println("failed to write output to file, reason -> ", err)
	}
	log.Printf("finished thread dump, pid: %d\n", pid)
}
