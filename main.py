#!/usr/bin/env python3
"""Launcher for the gitforge entry: exec the bundled static Go binary."""
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.join(HERE, "forge")

os.execv(BIN, [BIN] + sys.argv[1:])
