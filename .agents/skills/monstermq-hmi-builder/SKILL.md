---
name: monstermq-hmi-builder
description: >
  Guide for creating, editing, and packaging HTML/JS/CSS HMI apps and dashboards served by the MonsterMQ Edge broker.
  Use this skill whenever building industrial HMIs, SCADA screens, live topic charts, or telemetry dashboards hosted at /hmi/.
  Trigger on mentions of "create HMI", "build dashboard", "HMI screen", "live chart", "topic history", "WinCC", "web HMI",
  or when generating HTML apps for MonsterMQ Edge.
---

# MonsterMQ Edge HMI Builder Skill

This skill provides comprehensive instructions, architecture patterns, UI design guidelines, and copy-paste boilerplate code for creating standalone, HTML/JS-based HMIs and dashboards hosted directly by the MonsterMQ Edge broker (e.g. running on Siemens WinCC Unified Comfort Panels or Raspberry Pi).

---

## 1. HMI Hosting Architecture & Routing

- **Base Directory**: `./data/hmi/`
- **Dashboard Structure**: Every dashboard app lives in its own subdirectory under `./data/hmi/<dashboardname>/` containing `index.html` and assets.
- **Main Dashboard**: Served at `http://<broker>:4000/hmi/` (alias for the designated main dashboard app, default: `main`).
- **Named Dashboards**: Served at `http://<broker>:4000/hmi/<dashboardname>/`.

---

## 2. Broker Data Access (GraphQL API)

All HMIs communicate directly with the broker's GraphQL endpoint (`/graphql`) over HTTP and WebSockets.

### 2.1 Fetching Current Topic Values
```javascript
async function getTopicValue(topic) {
    const query = `
        query {
            currentValue(topic: "${topic}") {
                topic
                payload
                timestamp
            }
        }
    `;
    const res = await fetch('/graphql', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ query })
    });
    const data = await res.json();
    return data.data.currentValue;
}
```

### 2.2 Reading Historical Messages
```javascript
async function getHistory(topicFilter, limit = 100) {
    const query = `
        query {
            archivedMessages(topicFilter: "${topicFilter}", limit: ${limit}) {
                topic
                payload
                timestamp
            }
        }
    `;
    const res = await fetch('/graphql', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ query })
    });
    const data = await res.json();
    return data.data.archivedMessages || [];
}
```

### 2.3 Publishing Commands / Control Signals
```javascript
async function publishControl(topic, payload) {
    const query = `
        mutation($input: PublishInput!) {
            publish(input: $input) {
                success
                message
            }
        }
    `;
    const variables = {
        input: {
            topic: topic,
            payload: typeof payload === 'object' ? JSON.stringify(payload) : String(payload),
            qos: 1,
            retain: false
        }
    };
    const res = await fetch('/graphql', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ query, variables })
    });
    return (await res.json()).data.publish;
}
```

### 2.4 Real-time Subscriptions (GraphQL WebSocket)
```javascript
function subscribeTopicUpdates(topicFilters, onUpdate) {
    const wsUrl = (window.location.protocol === 'https:' ? 'wss://' : 'ws://') + window.location.host + '/graphql';
    const socket = new WebSocket(wsUrl, 'graphql-ws');

    socket.onopen = () => {
        // Connection init
        socket.send(JSON.stringify({ type: 'connection_init' }));
    };

    socket.onmessage = (event) => {
        const msg = JSON.parse(event.data);
        if (msg.type === 'connection_ack') {
            // Subscribe payload
            socket.send(JSON.stringify({
                id: '1',
                type: 'start',
                payload: {
                    query: `
                        subscription {
                            topicUpdates(topicFilters: ${JSON.stringify(topicFilters)}) {
                                topic
                                payload
                                timestamp
                            }
                        }
                    `
                }
            }));
        } else if (msg.type === 'data' && msg.payload && msg.payload.data) {
            onUpdate(msg.payload.data.topicUpdates);
        }
    };

    return socket;
}
```

---

## 3. Industrial UI Design Tokens & Theme

To ensure HMIs feel premium, modern, and readable on industrial panels (Siemens WinCC Comfort Panels), follow this color palette and layout standard:

- **Background**: `#0f172a` (Slate 900)
- **Card Container**: `#1e293b` (Slate 800) with `border: 1px solid #334155`
- **Primary Text**: `#f8fafc` (Slate 50)
- **Secondary Text**: `#94a3b8` (Slate 400)
- **Accent Primary**: `#38bdf8` (Sky 400)
- **Status OK**: `#22c55e` (Emerald 500)
- **Status Warning**: `#f59e0b` (Amber 500)
- **Status Alarm**: `#ef4444` (Red 500)

---

## 4. Complete Boilerplate Dashboard Template

When creating a new HMI screen, generate a self-contained single-file HTML like the one below:

```html
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Industrial Monitoring HMI</title>
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0f172a; color: #f8fafc; padding: 1.5rem; }
        .header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 1.5rem; padding-bottom: 1rem; border-bottom: 1px solid #334155; }
        .title { font-size: 1.5rem; font-weight: 600; color: #38bdf8; }
        .status-badge { background: #064e3b; color: #34d399; padding: 0.25rem 0.75rem; border-radius: 9999px; font-size: 0.875rem; font-weight: 500; }
        .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 1.25rem; margin-bottom: 1.5rem; }
        .card { background: #1e293b; border: 1px solid #334155; border-radius: 8px; padding: 1.25rem; }
        .card-label { font-size: 0.875rem; color: #94a3b8; margin-bottom: 0.5rem; }
        .card-value { font-size: 2.25rem; font-weight: 700; color: #f8fafc; }
        .card-unit { font-size: 1rem; color: #64748b; font-weight: 400; }
        .chart-container { background: #1e293b; border: 1px solid #334155; border-radius: 8px; padding: 1.25rem; height: 350px; }
        .btn { background: #0284c7; color: white; border: none; padding: 0.6rem 1.2rem; border-radius: 6px; font-weight: 500; cursor: pointer; transition: background 0.2s; }
        .btn:hover { background: #0369a1; }
        .btn:active { transform: scale(0.98); }
    </style>
</head>
<body>
    <div class="header">
        <div class="title">Production Line A — Live Telemetry</div>
        <div class="status-badge" id="connStatus">CONNECTED</div>
    </div>

    <div class="grid">
        <div class="card">
            <div class="card-label">Temperature</div>
            <div class="card-value" id="tempVal">-- <span class="card-unit">°C</span></div>
        </div>
        <div class="card">
            <div class="card-label">Pressure</div>
            <div class="card-value" id="pressVal">-- <span class="card-unit">bar</span></div>
        </div>
        <div class="card">
            <div class="card-label">Motor Speed</div>
            <div class="card-value" id="speedVal">-- <span class="card-unit">RPM</span></div>
        </div>
    </div>

    <div class="chart-container">
        <canvas id="lineChart"></canvas>
    </div>

    <script>
        // Init Line Chart
        const ctx = document.getElementById('lineChart').getContext('2d');
        const chart = new Chart(ctx, {
            type: 'line',
            data: {
                labels: [],
                datasets: [{
                    label: 'Temperature (°C)',
                    data: [],
                    borderColor: '#38bdf8',
                    backgroundColor: 'rgba(56, 189, 248, 0.1)',
                    fill: true,
                    tension: 0.3
                }]
            },
            options: {
                responsive: true,
                maintainAspectRatio: false,
                plugins: { legend: { labels: { color: '#94a3b8' } } },
                scales: {
                    x: { ticks: { color: '#64748b' }, grid: { color: '#334155' } },
                    y: { ticks: { color: '#64748b' }, grid: { color: '#334155' } }
                }
            }
        });

        // Live Subscriptions via GraphQL WS
        const wsUrl = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/graphql';
        const ws = new WebSocket(wsUrl, 'graphql-ws');

        ws.onopen = () => ws.send(JSON.stringify({ type: 'connection_init' }));
        ws.onmessage = (e) => {
            const msg = JSON.parse(e.data);
            if (msg.type === 'connection_ack') {
                ws.send(JSON.stringify({
                    id: '1',
                    type: 'start',
                    payload: {
                        query: `subscription { topicUpdates(topicFilters: ["telemetry/#"]) { topic payload timestamp } }`
                    }
                }));
            } else if (msg.type === 'data' && msg.payload && msg.payload.data) {
                const update = msg.payload.data.topicUpdates;
                handleTopicUpdate(update.topic, update.payload, update.timestamp);
            }
        };

        function handleTopicUpdate(topic, payloadStr, timestamp) {
            try {
                const val = typeof payloadStr === 'string' ? JSON.parse(payloadStr) : payloadStr;
                const timeStr = new Date(timestamp).toLocaleTimeString();

                if (topic.endsWith('/temperature')) {
                    const temp = typeof val === 'object' ? val.value : val;
                    document.getElementById('tempVal').childNodes[0].nodeValue = temp.toFixed(1) + ' ';
                    
                    chart.data.labels.push(timeStr);
                    chart.data.datasets[0].data.push(temp);
                    if (chart.data.labels.length > 20) {
                        chart.data.labels.shift();
                        chart.data.datasets[0].data.shift();
                    }
                    chart.update();
                }
            } catch (err) { console.error('Parse error', err); }
        }
    </script>
</body>
</html>
```

---

## 5. MCP Server Tool Reference for AI Agents

AI assistants can interact with MonsterMQ Edge via the embedded MCP Server (`http://<broker>:3000/sse`) using these tools:

1. `list_dashboards()`: Returns all installed HMI apps and indicates which one is `isMain`.
2. `create_dashboard(name: string, set_as_main: bool)`: Creates a new dashboard folder under `./data/hmi/<name>/`.
3. `delete_dashboard(name: string)`: Removes a dashboard app.
4. `set_main_dashboard(name: string)`: Sets the specified dashboard as the primary one served at `/hmi/`.
5. `list_hmi_files(dashboard: string)`: Lists all files in a dashboard directory.
6. `read_hmi_file(dashboard: string, path: string)`: Reads an HTML/JS/CSS file.
7. `write_hmi_file(dashboard: string, path: string, content: string)`: Writes/deploys an HTML/JS/CSS file directly to the edge broker.
