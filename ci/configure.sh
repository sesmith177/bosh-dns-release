#!/usr/bin/env bash

fly -t ${CONCOURSE_TARGET:-production} sp -p bosh-dns-release -c ./ci/pipeline.yml --load-vars-from <(lpass show 'dns-release pipeline vars' --notes)
