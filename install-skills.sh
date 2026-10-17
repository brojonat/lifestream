#!/bin/bash
# Per-skill installs, selected to match the capabilities in README.md.
# Commit the resulting skills-lock.json.
#
# No Pi / Pi Durable skill: `npx skills find` (2026-10-17) only turned up
# small third-party ones. Read Earendil's docs and source instead (TODO 16).
set -e

npx skills add brojonat/llmsrules -s go-service -y
npx skills add brojonat/llmsrules -s urfave-cli -y
npx skills add brojonat/llmsrules -s datastar -y
npx skills add brojonat/llmsrules -s air-go -y
npx skills add brojonat/llmsrules -s sqlite -y
npx skills add brojonat/llmsrules -s litestream-k8s -y
npx skills add brojonat/llmsrules -s k8s-deployment -y
npx skills add obra/superpowers -s systematic-debugging -y
npx skills add obra/superpowers -s test-driven-development -y
