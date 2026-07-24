# Todo

* [x] need to pass mocker config from scenario file to k8s/mocker-deployment.yaml
* [benchmark: add run verdict for steady-state backend validity](https://github.com/volcano-sh/kthena/issues/1271)
   * state a head of traffic: support `ModelServer` status
   * state after traffic: check `ModelServer` restart times
* verify metrics_collector.py on /metrics endpoint
   * introduce pprof collection
* re-design core model of test scenario, to proof the value of benchmark testing
* Second stage, start optimization-oriented testing
