#!/usr/bin/env bash
# scripts/lib/ssm-common.sh -- shared helpers for the ssm-*.sh wrappers.
#
# Sourced (not executed) by scripts/ssm-{shell,profile,sweep,bench}.sh.
# Provides:
#   cf_outputs        cache the CFN outputs of the bench stack
#   stack_output      read a single output by key
#   send_and_wait     send an SSM command, poll until done, dump stdout
#   pull_s3           sync a prefix from the results bucket into a local dir
#
# Environment knobs honoured everywhere:
#   AWS_REGION       (default us-east-1)
#   HF_STACK_NAME    (default PulsysBench)

set -euo pipefail
: "${AWS_REGION:=us-east-1}"
: "${HF_STACK_NAME:=PulsysBench}"

_outputs_json=""
cf_outputs() {
    if [ -z "$_outputs_json" ]; then
        _outputs_json="$(
            aws cloudformation describe-stacks \
                --region "$AWS_REGION" \
                --stack-name "$HF_STACK_NAME" \
                --query 'Stacks[0].Outputs' \
                --output json
        )"
        if [ -z "$_outputs_json" ] || [ "$_outputs_json" = "null" ]; then
            echo "FATAL: stack $HF_STACK_NAME has no outputs in $AWS_REGION" >&2
            exit 1
        fi
    fi
    printf '%s' "$_outputs_json"
}

stack_output() {
    local key="$1"
    cf_outputs | jq -r --arg k "$key" '.[] | select(.OutputKey==$k) | .OutputValue'
}

# Args: doc-name [param1=value1 ...]
# Echoes the command ID on success, exits non-zero on dispatch error.
ssm_send() {
    local doc="$1"; shift
    local instance
    instance="$(stack_output InstanceIdOut)"
    if [ -z "$instance" ]; then
        echo "FATAL: could not resolve InstanceIdOut from stack $HF_STACK_NAME" >&2
        exit 1
    fi

    local params_json='{}'
    if [ "$#" -gt 0 ]; then
        # Convert key=value pairs into a JSON object with string-array values
        # (SSM parameters are arrays even for single values).
        params_json="$(
            for kv in "$@"; do
                k="${kv%%=*}"
                v="${kv#*=}"
                jq -n --arg k "$k" --arg v "$v" '{($k): [$v]}'
            done | jq -s 'reduce .[] as $x ({}; . * $x)'
        )"
    fi

    aws ssm send-command \
        --region "$AWS_REGION" \
        --instance-ids "$instance" \
        --document-name "$doc" \
        --parameters "$params_json" \
        --cloud-watch-output-config CloudWatchOutputEnabled=true \
        --query 'Command.CommandId' \
        --output text
}

# Poll an SSM command to completion and print stdout to current shell stdout.
# Args: command-id
ssm_wait() {
    local cmd_id="$1"
    local instance
    instance="$(stack_output InstanceIdOut)"
    echo "==> waiting for SSM command $cmd_id (instance $instance)" >&2

    # The list-command-invocations call doesn't expose Status nicely for
    # a single invocation, but get-command-invocation does -- it expects
    # both the command id and the instance id.
    local status
    while true; do
        status="$(
            aws ssm get-command-invocation \
                --region "$AWS_REGION" \
                --command-id "$cmd_id" \
                --instance-id "$instance" \
                --query 'Status' \
                --output text 2>/dev/null || echo "InProgress"
        )"
        case "$status" in
            Success|Failed|Cancelled|TimedOut)
                break
                ;;
            Pending|InProgress|Delayed)
                sleep 3
                ;;
            *)
                echo "==> unexpected SSM status: $status" >&2
                sleep 3
                ;;
        esac
    done

    aws ssm get-command-invocation \
        --region "$AWS_REGION" \
        --command-id "$cmd_id" \
        --instance-id "$instance" \
        --query 'StandardOutputContent' \
        --output text

    if [ "$status" != "Success" ]; then
        echo "==> SSM command finished with status $status" >&2
        aws ssm get-command-invocation \
            --region "$AWS_REGION" \
            --command-id "$cmd_id" \
            --instance-id "$instance" \
            --query 'StandardErrorContent' \
            --output text >&2
        exit 1
    fi
}

# Pull artifacts from the results bucket.
# Args: prefix local-dest
pull_s3() {
    local prefix="$1"
    local dest="$2"
    local bucket
    bucket="$(stack_output ResultsBucketOut)"
    if [ -z "$bucket" ]; then
        echo "FATAL: could not resolve ResultsBucketOut" >&2
        exit 1
    fi
    mkdir -p "$dest"
    aws s3 cp \
        --region "$AWS_REGION" \
        --recursive \
        "s3://$bucket/$prefix" \
        "$dest"
}
