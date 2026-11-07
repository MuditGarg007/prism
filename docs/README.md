# Prism documentation

Prism is a reverse proxy in the style of nginx, written in Go. These docs explain how it works, and along the way how real backend infrastructure works: load balancing, rate limiting, retries, circuit breakers, HTTP caching, hot reloads and more.

You should already know how to program and be able to read Go. You don't need to know anything about networking or proxies. Terms are defined in the [glossary](glossary.md).

## Start here

Read these in order. Together they take about two hours, and you finish knowing what every part of the codebase is for.

1. [What is a reverse proxy?](learn/01-what-is-a-reverse-proxy.md) The problem Prism solves, and the idea it is built on.
2. [HTTP for proxies](learn/02-http-for-proxies.md) The parts of HTTP that matter once something sits between client and server.
3. [Run it](learn/03-run-it.md) Start Prism on your machine, then break things and watch what happens.
4. [Codebase tour](learn/04-codebase-tour.md) The packages, the core types, and what happens at startup.
5. [Life of a request](learn/05-life-of-a-request.md) One request followed through the code, from the socket and back.

## How the docs are organized

```mermaid
flowchart LR
    A[learn/<br/>read in order] --> B[concepts/<br/>one topic each, any order]
    B --> C[guides/<br/>doing a job]
    B --> D[reference/<br/>looking something up]
```

| section | what it is for | status |
| --- | --- | --- |
| `learn/` | building a mental model, start to finish | done |
| `concepts/` | one subsystem at a time: the problem, the idea, Prism's code, an experiment, the tradeoffs | coming next |
| `guides/` | adding a feature, testing, running Prism in production | planned |
| `reference/` | every config key, admin endpoint, metric and status code | planned |

Until `reference/` exists, the commented [`examples/prism.yaml`](../examples/prism.yaml) lists every setting.

## Other documents

- [Benchmark results](bench/results.md) compare Prism with nginx.
- [Project plan](project-plan.md) records how Prism was built, milestone by milestone. It is history: where it disagrees with the code, the code is right.
