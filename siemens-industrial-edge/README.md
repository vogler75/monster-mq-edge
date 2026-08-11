# MonsterMQ-Edge - Siemens Industrial Edge Publisher Project

This repository branch/directory contains the exported application project for **MonsterMQ-Edge** for use with the **Siemens Industrial Edge Publisher**.

## Overview

MonsterMQ-Edge is a lightweight, single-binary MQTT broker tailored for edge devices such as Siemens SIMATIC WinCC Unified Comfort Panels. This directory provides the Industrial Edge Publisher structure needed to package and deploy MonsterMQ-Edge to Industrial Edge environments.

---

## Packaging and Importing into Industrial Edge Publisher

To import this project into the **Siemens Industrial Edge Publisher**:

1. **Create the TAR archive**:
   Package the `MonsterMQ-Edge` directory into a `.tar` archive:
   ```bash
   tar cvf MonsterMQ-Edge.tar MonsterMQ-Edge
   ```

2. **Import into Industrial Edge Publisher**:
   Open Siemens Industrial Edge Publisher and import the `MonsterMQ-Edge.tar` file.

3. **Prerequisite Docker Image**:
   Ensure that the required Docker image (`rocworks/monstermq-edge:latest`) is created and accessible by the Industrial Edge Publisher (either in a local registry or Docker Hub).

4. **Create the Edge App**:
   Use the Industrial Edge Publisher to build and export the final Industrial Edge app (`.app`) targeted for WinCC Unified Comfort Panels.

---

## Installing Pre-built App on WinCC Unified Comfort Panel

If you prefer to deploy a pre-built package directly without using Industrial Edge Publisher:

1. **Download Release**:
   Locate and download `MonsterMQ-Edge.tar` from the GitHub [Releases](https://github.com/vogler75/monster-mq-edge/releases).

2. **Rename File**:
   Rename `MonsterMQ-Edge.tar` to `MonsterMQ-Edge.app` (change the file extension from `.tar` to `.app`).

3. **Deploy to Panel**:
   Upload `MonsterMQ-Edge.app` directly to your Siemens SIMATIC WinCC Unified Comfort Panel.
   > **Note:** Industrial Edge runtime must be enabled on the WinCC Unified Comfort Panel.

---

## Accessing and Managing the Broker

Once the MonsterMQ-Edge app is running on the panel:

- Access and configure the broker using the **MonsterMQ Dashboard Desktop app**.
- Download the MonsterMQ Dashboard Desktop app from the Releases section of the [MonsterMQ Repository](https://github.com/vogler75/monster-mq).
