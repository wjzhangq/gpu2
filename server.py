import json
import time
import argparse
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs

data_store = {}  # {id: {"report": data, "timestamp": last_update}}

HOSTNAME_MAP = {
    "user-ThinkStation-PX": "user-ThinkStation-PX1",
}

MODEL_NAME_MAP = {
    "NVIDIA RTX 5880 Ada Generation": "RTX 4080 Ada",
}


def cleanup():
    """Remove old data beyond 300 seconds (5 min)"""
    now = time.time()
    expired = [k for k, v in data_store.items() if now - v["timestamp"] > 300]
    for k in expired:
        del data_store[k]


def calculate_merge(ids=None):
    now = time.time()

    # Filter valid report in 30 seconds
    filtered = []
    if ids:
        ids = set(ids)
        for k, v in data_store.items():
            if k in ids and now - v["timestamp"] <= 30:
                filtered.append(v["report"])
    else:
        for v in data_store.values():
            if now - v["timestamp"] <= 30:
                filtered.append(v["report"])

    if not filtered:
        return {}

    # CPU
    total_cores = 0
    cpu_usage_sum = 0
    for item in filtered:
        for cpu in item.get("cpus", []):
            cores = cpu.get("cores", 0)
            total_cores += cores
            cpu_usage_sum += cpu.get("usage_percent", 0) * cores

    cpu_usage_percent = cpu_usage_sum / total_cores if total_cores > 0 else 0
    cpu_result = {
        "cores": total_cores,
        "usage_percent": round(cpu_usage_percent, 2),
    }

    # Memory
    total_mem = sum(i["memory"]["total_gb"] for i in filtered)
    used_mem = sum(i["memory"]["used_gb"] for i in filtered)
    mem_result = {
        "total_gb": round(total_mem, 2),
        "used_gb": round(used_mem, 2),
        "usage_percent": round((used_mem / total_mem * 100) if total_mem else 0, 2),
    }

    # Disk
    total_disk = sum(d["total_gb"] for item in filtered for d in item.get("disks", []))
    used_disk = sum(d["used_gb"] for item in filtered for d in item.get("disks", []))
    disk_result = {
        "total_gb": round(total_disk, 2),
        "used_gb": round(used_disk, 2),
        "usage_percent": round((used_disk / total_disk * 100) if total_disk else 0, 2),
    }

    # GPU
    gpu_count = 0
    gpu_usage_percent_sum = 0
    gpu_mem_total = 0
    gpu_mem_used = 0
    for item in filtered:
        for gpu in item.get("gpus", []):
            gpu_count += 1
            gpu_usage_percent_sum += gpu.get("usage_percent", 0)
            gpu_mem_total += gpu.get("memory_total_gb", 0)
            gpu_mem_used += gpu.get("memory_used_gb", 0)

    gpu_result = {
        "memory_total_gb": round(gpu_mem_total, 2),
        "memory_used_gb": round(gpu_mem_used, 2),
        "memory_usage_percent": round((gpu_mem_used / gpu_mem_total * 100) if gpu_mem_total else 0, 2),
        "usage_percent": round((gpu_usage_percent_sum / gpu_count) if gpu_count else 0, 2),
    }

    return {
        "cpus": cpu_result,
        "memory": mem_result,
        "disk": disk_result,
        "gpus": gpu_result,
    }


class RequestHandler(BaseHTTPRequestHandler):

    def _send(self, status, body):
        origin = self.headers.get("Origin")
        allow_origin = origin if origin else "*"
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Access-Control-Allow-Origin", allow_origin)
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type")        
        self.end_headers()
        self.wfile.write(json.dumps(body).encode("utf-8"))

    def do_POST(self):
        if self.path == "/report":
            try:
                length = int(self.headers.get("Content-Length"))
                payload = self.rfile.read(length).decode("utf-8")
                report = json.loads(payload)
                machine_id = report.get("id")

                if not machine_id:
                    return self._send(400, {"error": "missing id"})

                # Insert or update
                data_store[machine_id] = {
                    "report": report,
                    "timestamp": time.time()
                }
                cleanup()
                return self._send(200, {"status": "ok"})
            except:
                return self._send(500, {"error": "invalid json"})

        return self._send(404, {"error": "Not found"})

    def do_GET(self):
        parsed = urlparse(self.path)

        if parsed.path == "/all":
            cleanup()
            now = time.time()
            result = []
            for k, v in data_store.items():
                item = v["report"].copy()
                item["offline"] = (now - v["timestamp"] > 30)
                if item["hostname"] in HOSTNAME_MAP:
                    item['old_hostname'] = item["hostname"]
                    item["hostname"] = HOSTNAME_MAP[item["hostname"]]
                for index, gpu in enumerate(item.get("gpus", [])):
                    print(gpu)
                    if gpu.get("model") in MODEL_NAME_MAP:
                        item["gpus"][index]['old_model'] = gpu["model"]
                        item["gpus"][index]["model"] = MODEL_NAME_MAP[gpu["model"]]
                result.append(item)
            return self._send(200, result)

        if parsed.path == "/merge":
            cleanup()
            params = parse_qs(parsed.query)
            ids = params.get("ids")
            if ids:
                ids = ids[0].split(",")
            result = calculate_merge(ids)
            return self._send(200, result)

        return self._send(404, {"error": "Not found"})


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=False, default=3000, help="Port to run the server on" )
    args = parser.parse_args()

    server = HTTPServer(("0.0.0.0", args.port), RequestHandler)
    print(f"Server running at http://0.0.0.0:{args.port}")
    server.serve_forever()
