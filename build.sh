#!/bin/bash

image_group="registry.digitalocean.com/ovr"
image_name="brotkasten"
image_tag="0.0.2"

docker build . -t "${image_group}/${image_name}:${image_tag}"
docker tag "${image_group}/${image_name}:${image_tag}" "${image_group}/${image_name}:latest"

docker push "${image_group}/${image_name}:${image_tag}" 
docker push "${image_group}/${image_name}:latest"