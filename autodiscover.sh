#!/bin/bash

# Allow running under callers that enable set -u (nounset).
# These vars are optional inputs and may be unset.
: "${LOCAL_IP:=}"
: "${ETH_IF:=}"
: "${IB_IF:=}"
: "${NODES_ARG:=}"

# Function to detect IB and Ethernet interfaces
detect_interfaces() {
    # If both interfaces are already set, nothing to do
    if [[ -n "$ETH_IF" && -n "$IB_IF" ]]; then
        return 0
    fi

    # Check for required tools
    if ! command -v ibdev2netdev &> /dev/null; then
        echo "Error: ibdev2netdev not found. Cannot auto-detect interfaces."
        return 1
    fi

    echo "Auto-detecting interfaces..."
    
    # Get all Up interfaces: "rocep1s0f1 port 1 ==> enp1s0f1np1 (Up)"
    # We capture: IB_DEV, NET_DEV
    mapfile -t IB_NET_PAIRS < <(ibdev2netdev | awk '/Up\)/ {print $1 " " $5}')
    
    if [ ${#IB_NET_PAIRS[@]} -eq 0 ]; then
        echo "Error: No active IB interfaces found."
        return 1
    fi

    DETECTED_IB_IFS=()
    CANDIDATE_ETH_IFS=()

    for pair in "${IB_NET_PAIRS[@]}"; do
        ib_dev=$(echo "$pair" | awk '{print $1}')
        net_dev=$(echo "$pair" | awk '{print $2}')
        
        DETECTED_IB_IFS+=("$ib_dev")
        
        # Check if interface has an IP address
        if ip addr show "$net_dev" | grep -q "inet "; then
            CANDIDATE_ETH_IFS+=("$net_dev")
        fi
    done

    # Set IB_IF if not provided
    if [[ -z "$IB_IF" ]]; then
        IB_IF=$(IFS=,; echo "${DETECTED_IB_IFS[*]}")
        echo "  Detected IB_IF: $IB_IF"
    fi

    # Set ETH_IF if not provided
    if [[ -z "$ETH_IF" ]]; then
        if [ ${#CANDIDATE_ETH_IFS[@]} -eq 0 ]; then
            echo "Error: No active IB-associated interfaces have IP addresses."
            return 1
        fi
        
        # Selection logic: Prefer interface without capital 'P'
        SELECTED_ETH=""
        for iface in "${CANDIDATE_ETH_IFS[@]}"; do
            if [[ "$iface" != *"P"* ]]; then
                SELECTED_ETH="$iface"
                break
            fi
        done
        
        # Fallback: Use the first one if all have 'P' or none found yet
        if [[ -z "$SELECTED_ETH" ]]; then
            SELECTED_ETH="${CANDIDATE_ETH_IFS[0]}"
        fi
        
        ETH_IF="$SELECTED_ETH"
        echo "  Detected ETH_IF: $ETH_IF"
    fi
}

# Function to detect local IP
detect_local_ip() {
    if [[ -n "$LOCAL_IP" ]]; then
        return 0
    fi

    # Ensure interface is detected if not provided
    if [[ -z "$ETH_IF" ]]; then
        detect_interfaces || return 1
    fi

    # Get CIDR of the selected ETH_IF
    CIDR=$(ip -o -f inet addr show "$ETH_IF" | awk '{print $4}' | head -n 1)
    
    if [[ -z "$CIDR" ]]; then
        echo "Error: Could not determine IP/CIDR for interface $ETH_IF"
        return 1
    fi
    
    LOCAL_IP=${CIDR%/*}
    echo "  Detected Local IP: $LOCAL_IP ($CIDR)"
}

# Function to detect cluster nodes
detect_nodes() {
    detect_local_ip || return 1

    # If nodes are already set, populate PEER_NODES and return
    if [[ -n "$NODES_ARG" ]]; then
        PEER_NODES=()
        IFS=',' read -ra ALL_NODES <<< "$NODES_ARG"
        for node in "${ALL_NODES[@]}"; do
            node=$(echo "$node" | xargs)
            if [[ "$node" != "$LOCAL_IP" ]]; then
                PEER_NODES+=("$node")
            fi
        done
        return 0
    fi

    echo "Auto-detecting nodes..."
    
    if ! command -v nc &> /dev/null; then
        echo "Error: nc (netcat) not found. Please install netcat."
        return 1
    fi
    
    if ! command -v python3 &> /dev/null; then
        echo "Error: python3 not found. Please install python3."
        return 1
    fi

    DETECTED_IPS=("$LOCAL_IP")
    PEER_NODES=()
    
    echo "  Scanning for SSH peers on $CIDR..."
    
    # Generate list of IPs using python
    ALL_IPS=$(python3 -c "import ipaddress, sys; [print(ip) for ip in ipaddress.ip_network(sys.argv[1], strict=False).hosts()]" "$CIDR")
    
    TEMP_IPS_FILE=$(mktemp)
    
    # Scan in parallel
    for ip in $ALL_IPS; do
        # Skip own IP
        if [[ "$ip" == "$LOCAL_IP" ]]; then continue; fi
        
        (
            # Check port 22 with 1 second timeout
            if nc -z -w 1 "$ip" 22 &>/dev/null; then
                echo "$ip" >> "$TEMP_IPS_FILE"
            fi
        ) &
    done
    
    # Wait for all background scans to complete
    wait
    
    # Read found IPs
    if [[ -f "$TEMP_IPS_FILE" ]]; then
        while read -r ip; do
             DETECTED_IPS+=("$ip")
             PEER_NODES+=("$ip")
             echo "  Found peer: $ip"
        done < "$TEMP_IPS_FILE"
        rm -f "$TEMP_IPS_FILE"
    fi
    
    # Sort IPs
    IFS=$'\n' SORTED_IPS=($(sort <<<"${DETECTED_IPS[*]}"))
    unset IFS
    
    NODES_ARG=$(IFS=,; echo "${SORTED_IPS[*]}")
    echo "  Cluster Nodes: $NODES_ARG"
}
