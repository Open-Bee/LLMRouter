import argparse
import os
import random
import re
import subprocess
import time
from datetime import datetime
from functools import wraps

import yaml


def print_info(msg):
    """Print information with timestamp"""
    print(f"{datetime.now().strftime('%Y-%m-%d %H:%M:%S')} {msg}", flush=True)


def stage_monitor(stage_name):
    """
    Decorator: monitors execution time and status of a deployment stage.

    Args:
        stage_name (str): Stage name
    """

    def decorator(func):

        @wraps(func)
        def wrapper(*args, **kwargs):
            print_info(f"\n{'='*60}")
            print_info(f"{stage_name}")
            print_info(f"{'='*60}")

            start_time = time.time()
            try:
                result = func(*args, **kwargs)
                elapsed = int(time.time() - start_time)
                print_info(f"\n✓ {stage_name} completed, elapsed: {elapsed}s")
                return result
            except Exception as e:
                elapsed = int(time.time() - start_time)
                print_info(f"\n✗ {stage_name} failed, elapsed: {elapsed}s")
                print_info(f"Error: {e}")
                raise

        return wrapper

    return decorator


def get_hosts(hosts_arg=None):
    """
    Get list of host IP addresses.

    Args:
        hosts_arg (str, optional): Comma-separated list of hosts, if provided will use this instead of environment variable

    Returns:
        list: List of host IP addresses
    """
    # Prioritize using parameter-specified host list
    if hosts_arg:
        node_ip_list = hosts_arg
        print_info(f"Using parameter-specified host list")
    else:
        node_ip_list = os.environ.get("NODE_IP_LIST")
        if not node_ip_list:
            print_info(
                "Error: Environment variable NODE_IP_LIST not set and no hosts specified via parameter"
            )
            return []
        print_info(f"Using environment variable NODE_IP_LIST")

    # Convert comma-separated list to space-separated and remove ":[int]" suffix
    formatted_hosts = re.sub(r":\d+", "", node_ip_list.replace(",", " ")).split(" ")
    return [host.strip() for host in formatted_hosts if host.strip()]


def check_port_on_remote_host(host, port):
    """
    Check whether a port is available on a remote host.
    Uses lsof to check if the port is occupied by a process.

    Note: If the check times out, returns True (assumes port is available).
    """
    try:
        # Use lsof to check if the port is occupied
        cmd = f'export PDSH_RCMD_TYPE=ssh; pdsh -f 128 -w {host} "lsof -i:{port} -t" 2>/dev/null'
        result = subprocess.run(
            cmd, shell=True, capture_output=True, text=True, timeout=60
        )

        # If lsof finds the port is occupied, return unavailable
        if result.stdout.strip():
            return False

        return True
    except subprocess.TimeoutExpired:
        # On timeout, assume port is available
        print_info(f"  ⚠ Port check timed out (host={host}, port={port}), assuming available")
        return True
    except Exception as e:
        # On other exceptions, also assume port is available to avoid blocking
        print_info(f"  ⚠ Port check error (host={host}, port={port}): {e}, assuming available")
        return True


def find_available_port(host, start_port, max_attempts=100):
    """
    Find an available port on a host using remote lsof check.

    Raises:
        TimeoutError: When port check times out
        Exception: Other check errors
    """
    for i in range(max_attempts):
        port = start_port + i
        # Use remote host check for more reliable port detection
        if check_port_on_remote_host(host, port):
            return port
    return None


def get_model_name_from_service(host, port, timeout=5):
    """
    Get model name from vllm/sglang service (using OpenAI client).

    Args:
        host (str): Host address
        port (int): Service port
        timeout (int): Request timeout in seconds

    Returns:
        str: Model name, returns None if failed
    """
    try:
        # Use subprocess to call python to avoid openai import issues
        cmd = f"""python3 -c "
from openai import OpenAI
import sys
try:
    client = OpenAI(
        api_key='dummy',
        base_url='http://{host}:{port}/v1',
        timeout={timeout}
    )
    models = client.models.list()
    if models.data and len(models.data) > 0:
        print(models.data[0].id)
    else:
        sys.exit(1)
except Exception as e:
    sys.exit(1)
" 2>/dev/null"""

        result = subprocess.run(
            cmd, shell=True, capture_output=True, text=True, timeout=timeout + 2
        )
        if result.returncode == 0 and result.stdout.strip():
            return result.stdout.strip()
        return None
    except Exception:
        return None


def check_log_for_port_error(log_file):
    """
    Check log file for port-in-use errors (non-blocking, only checks current state).

    Args:
        log_file (str): Path to the log file

    Returns:
        tuple: (has_error, is_ready)
            - has_error: True if a port-in-use error is detected
            - is_ready: True if the service has started successfully
    """
    if not os.path.exists(log_file) or os.path.getsize(log_file) == 0:
        return False, False

    # Keywords to exclude known false positives (e.g. FlashInfer AllReduce workspace WARNING)
    false_positive_keywords = ["allreduce", "flashinfer"]

    try:
        with open(log_file, "r", errors="replace") as f:
            content = f.read()
            content_lower = content.lower()

            # Check each line for port-in-use errors, excluding known false positives
            has_error = False
            for line in content_lower.splitlines():
                if "address already in use" in line or "oserror: [errno 98]" in line:
                    # Skip lines containing known false positive keywords
                    if any(kw in line for kw in false_positive_keywords):
                        continue
                    has_error = True
                    break

            is_ready = (
                "application startup complete." in content_lower
                or "uvicorn running on" in content_lower
            )

            return has_error, is_ready
    except Exception as e:
        print_info(f"  Warning: Error reading log file {log_file}: {e}")
        return False, False


def kill_service_on_port(host, port):
    """
    Kill the process occupying a given port on a remote host.

    Args:
        host (str): Host address
        port (int): Port number
    """
    kill_cmds = [
        f'export PDSH_RCMD_TYPE=ssh; pdsh -f 128 -w {host} "fuser -k {port}/tcp" 2>/dev/null',
        f'export PDSH_RCMD_TYPE=ssh; pdsh -f 128 -w {host} "lsof -ti:{port} | xargs kill -9" 2>/dev/null',
    ]
    for cmd in kill_cmds:
        try:
            subprocess.run(cmd, shell=True, timeout=15)
            time.sleep(2)  # Wait for process cleanup
            return
        except subprocess.TimeoutExpired:
            print_info(f"  Warning: Kill command timed out on {host}:{port}, trying next method...")
            continue
        except Exception as e:
            print_info(f"  Warning: Error killing process on {host}:{port} - {e}")
            continue
    print_info(f"  Warning: All kill methods failed for {host}:{port}")


class DeploymentConfig:
    """Deployment configuration class, encapsulates all deployment-related parameters."""

    def __init__(
        self,
        model_name,
        start_port,
        tensor_parallel_size,
        python_cmd="python",
        gpus_per_node=8,
        log_dir="./log_dir",
        health_check_timeout=1800,
        hosts_arg=None,
        max_retry_per_service=5,
        backend="vllm",
        served_model_name=None,
    ):
        self.model_name = model_name
        self.start_port = start_port
        self.tensor_parallel_size = tensor_parallel_size
        self.python_cmd = python_cmd
        self.gpus_per_node = gpus_per_node
        self.log_dir = log_dir
        self.health_check_timeout = health_check_timeout
        self.hosts_arg = hosts_arg
        self.max_retry_per_service = max_retry_per_service
        self.backend = backend
        # If served_model_name is not provided, default to the last segment of model_name
        self.served_model_name = served_model_name or model_name.split("/")[-1]

        # Use served_model_name as the log directory name (strip special chars)
        self.model_log_name = self.served_model_name.replace("-", "_").lower()
        self.services_per_node = gpus_per_node // tensor_parallel_size
        self.cwd = os.getcwd()

    def validate(self):
        """Validate the configuration."""
        if self.services_per_node == 0:
            raise ValueError(
                f"tensor_parallel_size ({self.tensor_parallel_size}) "
                f"exceeds the number of GPUs per node ({self.gpus_per_node})"
            )
        return True


class ServiceInfo:
    """Service information class, encapsulates all info about a single service."""

    def __init__(
        self,
        task_id,
        host,
        port,
        host_idx,
        service_idx,
        gpu_ids,
        log_file,
        start_time=None,
        retry_count=0,
    ):
        self.task_id = task_id
        self.host = host
        self.port = port
        self.host_idx = host_idx
        self.service_idx = service_idx
        self.gpu_ids = gpu_ids
        self.log_file = log_file
        self.start_time = start_time or time.time()
        self.retry_count = retry_count

    def to_dict(self):
        """Convert to dictionary."""
        return {
            "task_id": self.task_id,
            "host": self.host,
            "port": self.port,
            "host_idx": self.host_idx,
            "service_idx": self.service_idx,
            "gpu_ids": self.gpu_ids,
            "log_file": self.log_file,
            "start_time": self.start_time,
            "retry_count": self.retry_count,
        }

    @classmethod
    def from_dict(cls, data):
        """Create instance from dictionary."""
        return cls(**data)


def allocate_host_ports(host, start_port, num_ports):
    """
    Allocate available ports for a given host.

    Args:
        host (str): Host address
        start_port (int): Starting port number
        num_ports (int): Number of ports to allocate

    Returns:
        list: List of available ports

    Raises:
        TimeoutError: When port check times out; the caller decides whether to skip the host
        Exception: Other errors
    """
    host_ports = []
    port_to_check = start_port

    for _ in range(num_ports):
        try:
            available_port = find_available_port(host, port_to_check)
        except TimeoutError:
            print_info(f"  ✗ Timeout: Port check timed out on host {host} port {port_to_check}")
            raise  # Re-raise for the caller to handle

        if available_port is None:
            print_info(
                f"  ✗ Warning: No available port found on host {host} (starting from {port_to_check})"
            )
            break
        host_ports.append(available_port)
        port_to_check = available_port + 1

    return host_ports


def start_single_service(service_info, config):
    """
    Start a single service.

    Args:
        service_info (ServiceInfo): Service information
        config (DeploymentConfig): Deployment configuration

    Returns:
        bool: Whether the service started successfully
    """
    cmd = build_deploy_command(
        host=service_info.host,
        cwd=config.cwd,
        python_cmd=config.python_cmd,
        gpu_ids=service_info.gpu_ids,
        port=service_info.port,
        log_file=service_info.log_file,
        model_name=config.model_name,
        tensor_parallel_size=config.tensor_parallel_size,
        backend=config.backend,
        served_model_name=config.served_model_name,
    )

    try:
        print_info(f"  Starting service: {cmd}")
        subprocess.run(cmd, shell=True, check=True)
        return True
    except Exception as e:
        print_info(f"  ✗ Failed to start service: {e}")
        return False


@stage_monitor("Phase 1: Async-launch all services at once")
def phase1_start_all_services(hosts, config):
    """
    Phase 1: Asynchronously start all services on all hosts.

    Args:
        hosts (list): List of hosts
        config (DeploymentConfig): Deployment configuration

    Returns:
        list: List of all launched ServiceInfo objects
    """
    all_services = []
    service_count = 0
    current_port = config.start_port

    for host_idx, host in enumerate(hosts):
        print_info(f"\nHost {host_idx+1}/{len(hosts)}: {host}")

        try:
            # Allocate ports for this host
            host_ports = allocate_host_ports(
                host, current_port, config.services_per_node
            )
        except Exception as e:
            print_info(f"  ✗ Error: Failed to allocate ports for host {host}: {e}")
            print_info(f"  Skipping host {host}, continuing with the next one")
            continue

        if len(host_ports) < config.services_per_node:
            print_info(
                f"  ⚠ Warning: Only {len(host_ports)} available ports found on {host}, "
                f"expected {config.services_per_node}"
            )

        if not host_ports:
            print_info(f"  ✗ Error: No available ports on host {host}, skipping")
            continue

        # Start multiple services on this host
        for service_idx, port in enumerate(host_ports):
            # Calculate GPU IDs
            start_gpu = service_idx * config.tensor_parallel_size
            gpu_ids = ",".join(
                str(start_gpu + i) for i in range(config.tensor_parallel_size)
            )

            # Generate log file path
            timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
            log_file = os.path.join(
                config.log_dir,
                f"{config.backend.upper()}_{config.model_log_name}",
                f"{timestamp}_{host_idx}_{service_idx}_port{port}.log",
            )
            os.makedirs(os.path.dirname(log_file), exist_ok=True)

            # Create service info object
            service_info = ServiceInfo(
                task_id=service_count + 1,
                host=host,
                port=port,
                host_idx=host_idx,
                service_idx=service_idx,
                gpu_ids=gpu_ids,
                log_file=log_file,
            )

            print_info(f"  Service {service_info.task_id}: GPU={gpu_ids}, Port={port}")

            # Start the service
            if start_single_service(service_info, config):
                print_info(f"  ✓ Service {service_info.task_id} launch command executed")
                all_services.append(service_info)

                # Stagger launches on the same host to avoid NCCL resource contention
                if service_idx < len(host_ports) - 1:
                    stagger_delay = 3
                    print_info(f"  Next instance on same host will start in {stagger_delay}s...")
                    time.sleep(stagger_delay)
            else:
                print_info(f"  ✗ Service {service_info.task_id} failed to start")

            service_count += 1

        # Update starting port for next host
        if host_ports:
            current_port = host_ports[-1] + 1

    return all_services


@stage_monitor("Phase 2: Poll logs and verify service startup")
def phase2_check_and_verify(all_services, config):
    """
    Phase 2: Poll all service logs, handle port conflicts, and verify startup.

    During log polling:
    - If port conflict detected → restart immediately
    - If service ready → verify model availability
    - Otherwise → keep waiting

    Args:
        all_services (list): List of all service info objects
        config (DeploymentConfig): Deployment configuration

    Returns:
        list: List of successfully deployed services (dict format)
    """
    print_info(f"Health check timeout: {config.health_check_timeout} seconds")
    print_info(f"Waiting 30 seconds for logs to be generated...\n")
    time.sleep(30)

    deployed_services = []
    pending_services = all_services.copy()
    start_check_time = time.time()
    check_interval = 20
    last_report_time = start_check_time

    while (
        pending_services
        and (time.time() - start_check_time) < config.health_check_timeout
    ):
        still_pending = []

        for service_info in pending_services:
            # Check log status
            has_error, is_ready = check_log_for_port_error(
                service_info.log_file
            )

            if has_error:
                # Port conflict detected, restart immediately
                if service_info.retry_count >= config.max_retry_per_service:
                    print_info(
                        f"❌ Service {service_info.task_id} ({service_info.host}:{service_info.port}) "
                        f"still has port conflict after {service_info.retry_count} retries, giving up"
                    )
                    continue

                print_info(
                    f"❌ Service {service_info.task_id} ({service_info.host}:{service_info.port}) "
                    f"port conflict, restarting... "
                    f"(attempt {service_info.retry_count + 1}/{config.max_retry_per_service})"
                )

                success = _restart_single_service(service_info, config)
                if success:
                    # Restart succeeded, continue monitoring
                    still_pending.append(service_info)
                else:
                    # Restart failed, stop monitoring this service
                    print_info(f"❌ Service {service_info.task_id} restart failed, giving up")

            elif is_ready:
                # Log shows service ready, verify model availability
                actual_model_name = get_model_name_from_service(
                    service_info.host, service_info.port, timeout=5
                )

                if actual_model_name is not None:
                    # Check if model name matches (warn only, not a failure condition)
                    model_name_matched = (
                        config.served_model_name in actual_model_name
                        or actual_model_name in config.served_model_name
                    )

                    elapsed = int(time.time() - service_info.start_time)
                    retry_info = (
                        f", retries: {service_info.retry_count}"
                        if service_info.retry_count > 0
                        else ""
                    )

                    if not model_name_matched:
                        print_info(
                            f"⚠️ Service {service_info.task_id} "
                            f"({service_info.host}:{service_info.port}) model name mismatch: "
                            f"configured={config.served_model_name}, actual={actual_model_name}"
                        )

                    print_info(
                        f"✓ Service {service_info.task_id} "
                        f"({service_info.host}:{service_info.port}) started successfully! "
                        f"Elapsed: {elapsed}s, model: {actual_model_name}{retry_info}"
                    )

                    # Add to deployed list
                    deployed_services.append(
                        {
                            "host": service_info.host,
                            "port": service_info.port,
                            "host_idx": service_info.host_idx,
                            "service_idx": service_info.service_idx,
                        }
                    )
                else:
                    # Log shows ready but API not responding yet, keep waiting
                    still_pending.append(service_info)
            else:
                # Neither error nor ready, keep waiting
                still_pending.append(service_info)

        # Update pending list
        pending_services = still_pending

        if pending_services:
            # Periodic progress report
            current_time = time.time()
            if current_time - last_report_time >= check_interval:
                elapsed_minutes = int((current_time - start_check_time) / 60)
                print_info(
                    f"⏳ {len(pending_services)} services still pending, "
                    f"waited {elapsed_minutes} minutes..."
                )
                last_report_time = current_time

            time.sleep(10)
        else:
            print_info(f"\n🎉 All services started successfully!")
            break

    # Check for timed-out services
    if pending_services:
        print_info(f"\n⚠️  The following {len(pending_services)} services timed out:")
        for service_info in pending_services:
            print_info(
                f"  ✗ Service {service_info.task_id} "
                f"({service_info.host}:{service_info.port})"
            )

    return deployed_services


def _restart_single_service(service_info, config):
    """
    Restart a single failed service (internal function).

    Each call performs one restart operation. Retry count is incremented via service_info.retry_count.
    The caller is responsible for checking whether retry_count has reached the maximum.

    Args:
        service_info (ServiceInfo): Service info to restart
        config (DeploymentConfig): Deployment configuration

    Returns:
        bool: Whether the restart was successful
    """
    # Kill the process occupying the port
    print_info(
        f"  Terminating service {service_info.task_id} at "
        f"{service_info.host}:{service_info.port}"
    )
    kill_service_on_port(service_info.host, service_info.port)

    # Find a new port
    new_port = find_available_port(
        service_info.host, service_info.port + random.randint(10, 50)
    )

    if new_port is None:
        print_info(f"  ❌ Cannot find a new port for service {service_info.task_id}")
        return False

    # Update service info
    service_info.port = new_port
    service_info.retry_count += 1
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    service_info.log_file = os.path.join(
        config.log_dir,
        f"{config.backend.upper()}_{config.model_log_name}",
        f"{timestamp}_{service_info.host_idx}_{service_info.service_idx}_"
        f"port{new_port}_retry{service_info.retry_count}.log",
    )
    os.makedirs(os.path.dirname(service_info.log_file), exist_ok=True)

    print_info(
        f"  Restarting service {service_info.task_id}: "
        f"attempt {service_info.retry_count}/{config.max_retry_per_service}, new port {new_port}"
    )

    # Restart the service
    if start_single_service(service_info, config):
        print_info(f"  ✓ Service {service_info.task_id} restart command executed")
        service_info.start_time = time.time()
        return True
    else:
        print_info(f"  ❌ Service {service_info.task_id} restart command failed")
        return False


def print_deployment_summary(deployed_services):
    """
    Print deployment summary.

    Args:
        deployed_services (list): List of deployed services
    """
    print_info(f"\n{'='*60}")
    print_info("Deployment Summary")
    print_info(f"{'='*60}")
    print_info(f"Total deployed: {len(deployed_services)} services")

    if deployed_services:
        ports = [s["port"] for s in deployed_services]
        print_info(f"Port range: {min(ports)} - {max(ports)}")
        print_info(f"\nAccess example:")
        print_info(
            f"  curl http://{deployed_services[0]['host']}:"
            f"{deployed_services[0]['port']}/v1/models"
        )


def _check_supports_vision(model_name):
    """Check if a model supports vision based on its name."""
    lower = model_name.lower()
    return "vl" in lower or "vision" in lower


def deploy_vllm_services(
    model_name,
    start_port,
    tensor_parallel_size,
    python_cmd="python",
    gpus_per_node=8,
    log_dir="./log_dir",
    health_check_timeout=1800,
    hosts_arg=None,
    max_retry_per_service=3,
    backend="vllm",
    served_model_name=None,
):
    """
    Deploy vLLM or SGLang services to multiple nodes.

    Args:
        model_name (str): Model name or path
        start_port (int): Starting port number
        tensor_parallel_size (int): Tensor parallel size per service
        python_cmd (str): Python interpreter path
        gpus_per_node (int): Number of GPUs per node
        log_dir (str): Log directory
        health_check_timeout (int): Health check timeout in seconds
        hosts_arg (str): Comma-separated host list
        max_retry_per_service (int): Max retries per service (default: 3)
        backend (str): Backend type, "vllm" or "sglang" (default: "vllm")
        served_model_name (str, optional): Model name exposed by the service; defaults to model_name.split("/")[-1]

    Returns:
        dict: Dictionary containing deployment info
    """
    # 1. Initialize and validate configuration
    hosts = get_hosts(hosts_arg)
    if not hosts:
        print_info("Error: Unable to get host list")
        return None

    # Create configuration object
    config = DeploymentConfig(
        model_name=model_name,
        start_port=start_port,
        tensor_parallel_size=tensor_parallel_size,
        python_cmd=python_cmd,
        gpus_per_node=gpus_per_node,
        log_dir=log_dir,
        health_check_timeout=health_check_timeout,
        hosts_arg=hosts_arg,
        max_retry_per_service=max_retry_per_service,
        backend=backend,
        served_model_name=served_model_name,
    )

    # Validate configuration
    try:
        config.validate()
    except ValueError as e:
        print_info(f"Configuration error: {e}")
        return None

    # Print deployment info
    print_info(f"\n{'='*60}")
    print_info(f"⚠️  IMPORTANT: Please ensure old {config.backend.upper()} services are stopped")
    print_info("   to avoid port conflicts!")
    print_info(f"{'='*60}\n")

    print_info(f"Backend: {config.backend.upper()}")
    print_info(f"Found {len(hosts)} hosts: {', '.join(hosts)}")
    print_info(f"Model: {config.model_name}")
    print_info(f"Served model name: {config.served_model_name}")
    print_info(f"Starting port: {config.start_port}")
    print_info(f"Tensor parallel size: {config.tensor_parallel_size}")
    print_info(f"GPUs per node: {config.gpus_per_node}")
    print_info(f"Services per node: {config.services_per_node}")

    # Ensure log directory exists
    os.makedirs(config.log_dir, exist_ok=True)

    # 2. Execute two-phase deployment
    try:
        # Phase 1: Start all services
        all_services = phase1_start_all_services(hosts, config)

        if not all_services:
            print_info("Error: No services started successfully")
            return None

        # Phase 2: Poll logs, handle port conflicts, and verify startup
        deployed_services = phase2_check_and_verify(all_services, config)

        # 3. Print deployment summary
        print_deployment_summary(deployed_services)

        return {"deployed_services": deployed_services}

    except Exception as e:
        print_info(f"\nError during deployment: {e}")
        import traceback

        traceback.print_exc()
        return None


def generate_router_config(
    deployed_services,
    model_name,
    served_model_name=None,
    output_file="router_config.yaml",
):
    """
    Generate a router_config.yaml file.

    Args:
        deployed_services (list): List of deployed services
        model_name (str): Model name
        served_model_name (str, optional): Model name exposed by the service; defaults to model_name.split("/")[-1]
        output_file (str): Output file path
    """
    if served_model_name is None:
        served_model_name = model_name.split("/")[-1]

    # Build model_list
    model_list = []
    for idx, service in enumerate(deployed_services):
        model_entry = {
            "model_name": served_model_name,
            "litellm_params": {
                "model": f"openai/{served_model_name}",
                "api_base": f'http://{service["host"]}:{service["port"]}/v1',
                "api_key": "dummy",
                "supports_vision": _check_supports_vision(served_model_name),
                "source_type": "self_deployed",
            },
        }
        model_list.append(model_entry)

    # Build complete config
    config = {
        "model_list": model_list,
        "router_settings": {
            "routing_strategy": "simple-shuffle",
            "num_retries": 5,
            "timeout": 3600,
        },
    }

    # Print config content
    print_info(f"\nGenerated Router Config content:")
    print_info(
        yaml.dump(config, default_flow_style=False, sort_keys=False, allow_unicode=True)
    )

    # Write to YAML file
    with open(output_file, "w") as f:
        yaml.dump(
            config, f, default_flow_style=False, sort_keys=False, allow_unicode=True
        )

    print_info(f"\n✓ Router config file generated: {output_file}")
    print_info(f"  Total instances: {len(deployed_services)}")


def _get_self_deployed_config_path():
    """Get the default path to self_deployed_config.yaml based on this script's location."""
    script_dir = os.path.dirname(os.path.abspath(__file__))
    return os.path.join(script_dir, "..", "configs", "self_deployed_config.yaml")


def append_to_self_deployed_config(
    deployed_services,
    model_name,
    served_model_name=None,
    config_file=None,
):
    """
    Incrementally append newly deployed services to self_deployed_config.yaml,
    with automatic deduplication based on api_base.

    Args:
        deployed_services (list): List of deployed services
        model_name (str): Model name
        served_model_name (str, optional): Model name exposed by the service
        config_file (str, optional): Config file path; defaults to configs/self_deployed_config.yaml
    """
    if served_model_name is None:
        served_model_name = model_name.split("/")[-1]

    if config_file is None:
        config_file = _get_self_deployed_config_path()

    # Read existing config
    existing_config = None
    if os.path.exists(config_file):
        try:
            with open(config_file, "r") as f:
                existing_config = yaml.safe_load(f)
        except Exception as e:
            print_info(f"⚠ Failed to read existing config file: {e}, will create a new one")

    if existing_config is None or "model_list" not in existing_config:
        existing_config = {"model_list": []}

    # Build api_base -> index mapping for quick lookup
    existing_api_base_idx = {}
    for idx, entry in enumerate(existing_config["model_list"]):
        params = entry.get("litellm_params", {})
        api_base = params.get("api_base")
        if api_base:
            existing_api_base_idx[api_base] = idx

    # Build new model entries: update existing ones, append new ones
    added_count = 0
    updated_count = 0
    for service in deployed_services:
        api_base = f'http://{service["host"]}:{service["port"]}/v1'

        model_entry = {
            "model_name": served_model_name,
            "litellm_params": {
                "model": f"openai/{served_model_name}",
                "api_base": api_base,
                "api_key": "dummy",
                "supports_vision": _check_supports_vision(served_model_name),
                "source_type": "self_deployed",
                "rpm_limit": None,
            },
        }

        if api_base in existing_api_base_idx:
            # Already exists, overwrite
            idx = existing_api_base_idx[api_base]
            existing_config["model_list"][idx] = model_entry
            print_info(f"  🔄 Updated existing service: {api_base}")
            updated_count += 1
        else:
            # Does not exist, append
            existing_config["model_list"].append(model_entry)
            existing_api_base_idx[api_base] = len(existing_config["model_list"]) - 1
            added_count += 1

    # Write back to config file
    os.makedirs(os.path.dirname(os.path.abspath(config_file)), exist_ok=True)
    with open(config_file, "w") as f:
        yaml.dump(
            existing_config,
            f,
            default_flow_style=False,
            sort_keys=False,
            allow_unicode=True,
        )

    print_info(
        f"\n✓ Updated {config_file}: added {added_count} services, "
        f"updated {updated_count} existing services, "
        f"total {len(existing_config['model_list'])} services"
    )


def build_deploy_command(
    host,
    cwd,
    python_cmd,
    gpu_ids,
    port,
    log_file,
    model_name,
    tensor_parallel_size,
    backend="vllm",
    served_model_name=None,
):
    """
    Build pdsh command for deploying vllm or sglang service.

    Args:
        host (str): Host address
        cwd (str): Working directory
        python_cmd (str): Python interpreter path
        gpu_ids (str): GPU ID list (comma-separated)
        port (int): Service port
        log_file (str): Log file path
        model_name (str): Model name
        tensor_parallel_size (int): Tensor parallel size
        backend (str): Backend type, "vllm" or "sglang" (default "vllm")
        served_model_name (str, optional): Model name exposed by the service

    Returns:
        str: Complete pdsh command
    """
    # If served_model_name is not provided, default to the last segment
    if served_model_name is None:
        served_model_name = model_name.split("/")[-1]

    # Check if this is a Qwen3.5 series model
    _model_id = (model_name + " " + (served_model_name or "")).lower()
    is_qwen3_5 = (
        "qwen3.5" in _model_id or "qwen3-5" in _model_id or "qwen3_5" in _model_id
    )

    # Common command parts
    cmd_parts = [
        f"export PDSH_RCMD_TYPE=ssh;",
        f'pdsh -f 128 -w {host} "',
        f"cd {cwd};",
        f"export CUDA_VISIBLE_DEVICES={gpu_ids};",
        # Increase NCCL timeout for parallel launches on the same node to avoid false failures
        f"export NCCL_TIMEOUT=360;",
        f"export OMP_NUM_THREADS=32;",
    ]

    if backend == "sglang":
        # SGLang specific command
        cmd_parts.extend(
            [
                f"export no_proxy=localhost,127.0.0.1,0.0.0.0,$no_proxy;",
                f"nohup {python_cmd} -m sglang.launch_server",
                f"--model-path {model_name}",
                f"--served-model-name {served_model_name}",
                f"--host 0.0.0.0",
                f"--port {port}",
                f"--tp {tensor_parallel_size}",
                f"--chunked-prefill-size 2048",
                f"--enable-metrics",
                f'> {log_file} 2>&1 &"',
            ]
        )
    else:  # vllm (default)
        # vLLM specific command
        vllm_parts = [
            f"export VLLM_WORKER_MULTIPROC_METHOD=spawn;",
            f"nohup {python_cmd} -m vllm.entrypoints.openai.api_server",
            f"--model {model_name}",
            f"--served-model-name {served_model_name}",
            f"--tensor-parallel-size {tensor_parallel_size}",
            f"--port {port}",
            f"--trust-remote-code",
            f"--enable-prefix-caching",
            f"--limit-mm-per-prompt.video 0",
            f"--async-scheduling",
        ]

        # Qwen3.5 series models need extra configuration
        if is_qwen3_5:
            vllm_parts.extend(
                [
                    f"--max-model-len 262144",
                    f"--reasoning-parser qwen3",
                    f"--mm-encoder-tp-mode data",
                ]
            )
        else:
            vllm_parts.extend(
                [
                    f"--max-model-len 65536",
                ]
            )
        # If model name contains -A{xx}B (e.g. -A3B), it's a MoE model; enable expert parallel
        if re.search(r"-a\d+b", _model_id):
            vllm_parts.append(f"--enable-expert-parallel")

        vllm_parts.append(f'> {log_file} 2>&1 &"')
        cmd_parts.extend(vllm_parts)

    return " ".join(cmd_parts)


def main():
    parser = argparse.ArgumentParser(
        description="Deploy vllm/sglang services on multiple nodes"
    )

    # Host configuration
    parser.add_argument(
        "--hosts",
        type=str,
        default=None,
        help='Comma-separated list of host IPs, e.g.: "192.168.1.1,192.168.1.2" (default: use NODE_IP_LIST environment variable)',
    )

    # Deployment mode parameters
    parser.add_argument(
        "--model-name",
        type=str,
        help="Model name or path (required for deployment mode)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=30596,
        help="Starting port number (required for deployment mode)",
    )
    parser.add_argument(
        "--tensor-parallel-size",
        type=int,
        help="Tensor parallel size for each service (required for deployment mode)",
    )
    parser.add_argument(
        "--max-num-seqs",
        type=int,
        default=180,
        help="vLLM max concurrent sequences (default: 180)",
    )
    parser.add_argument(
        "--python-cmd",
        type=str,
        default="python",
        help="Python interpreter path (default: python)",
    )
    parser.add_argument(
        "--gpus-per-node",
        type=int,
        default=8,
        help="Number of GPUs per node (default: 8)",
    )
    parser.add_argument(
        "--log-dir",
        type=str,
        default="./log_dir",
        help="Log directory (default: ./log_dir)",
    )
    parser.add_argument(
        "--health-check-timeout",
        type=int,
        default=3600,
        help="Service health check timeout in seconds (default: 3600)",
    )
    parser.add_argument(
        "--backend",
        type=str,
        default="vllm",
        choices=["vllm", "sglang"],
        help="Backend type: vllm or sglang (default: vllm)",
    )
    parser.add_argument(
        "--served-model-name",
        type=str,
        default=None,
        help="Served model name exposed by the service. If not provided, defaults to model_name.split('/')[-1]",
    )

    args = parser.parse_args()

    # Deployment mode - validate required parameters
    if not args.model_name:
        parser.error("Deployment mode requires --model-name parameter")
    if not args.port:
        parser.error("Deployment mode requires --port parameter")
    if not args.tensor_parallel_size:
        parser.error("Deployment mode requires --tensor-parallel-size parameter")

    # Convert python command path
    args.python_cmd = f"/root/anaconda3/envs/{args.python_cmd}/bin/python"

    result = deploy_vllm_services(
        model_name=args.model_name,
        start_port=args.port,
        tensor_parallel_size=args.tensor_parallel_size,
        python_cmd=args.python_cmd,
        gpus_per_node=args.gpus_per_node,
        log_dir=args.log_dir,
        health_check_timeout=args.health_check_timeout,
        hosts_arg=args.hosts,
        backend=args.backend,
        served_model_name=args.served_model_name,
    )

    if result and result["deployed_services"]:
        print_info(f"\n{'='*60}")
        print_info("Deployment completed!")
        print_info(f"{'='*60}")

        router_config_path = os.path.join(args.log_dir, "router_config_{backend}.yaml")
        generate_router_config(
            deployed_services=result["deployed_services"],
            model_name=args.model_name,
            served_model_name=args.served_model_name,
            output_file=router_config_path.format(backend=args.backend),
        )

        print_info(
            f"Router config generated: {router_config_path.format(backend=args.backend)}"
        )

        # Incrementally append to self_deployed_config.yaml
        append_to_self_deployed_config(
            deployed_services=result["deployed_services"],
            model_name=args.model_name,
            served_model_name=args.served_model_name,
        )
    else:
        print_info("\nEncountered issues during deployment, please check logs.")


if __name__ == "__main__":
    main()
"""
Usage Examples:

# 1. Deploy vLLM using NODE_IP_LIST environment variable
export NODE_IP_LIST="192.168.1.1,192.168.1.2,192.168.1.3"
python self_deploy.py --python-cmd vllm_0.14.0 --model-name Qwen/Qwen3-VL-235B-A22B-Instruct --port 22005 --tensor-parallel-size 8 --backend vllm --served-model-name Qwen3-VL-235B-A22B-Instruct

# 2. Deploy vLLM with specified host list
python self_deploy.py --hosts "192.168.1.100" --python-cmd vllm_0.14.0 --model-name Qwen/Qwen2.5-VL-32B-Instruct --port 30010 --tensor-parallel-size 4 --backend vllm

# 3. Deploy SGLang with specified host list
python self_deploy.py --python-cmd sglang_0.5.7 --model-name Qwen/Qwen3-VL-235B-A22B-Instruct --port 22005 --tensor-parallel-size 8 --backend sglang

# 4. Deploy vLLM with a local model path and custom served name
python self_deploy.py --python-cmd vllm_0.14.0 --model-name /path/to/model --port 22005 --tensor-parallel-size 4 --backend vllm --served-model-name Qwen3-235B-A22B-Instruct-2507-FP8

"""
