# Copyright The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import os
import subprocess
from pathlib import Path

from kthena.downloader.base import ModelDownloader
from kthena.downloader.logger import setup_logger

logger = setup_logger()


class PVCDownloader(ModelDownloader):
    def __init__(self, source_path: str = None):
        super().__init__()
        if not source_path:
            raise ValueError("PVC source URI must be provided")
        self.source_path = source_path

    @staticmethod
    def _copy_from_pvc(pvc_path: str, output_dir: str) -> bool:
        pvc_path_obj = Path(pvc_path).resolve()
        output_dir_obj = Path(output_dir).resolve()

        if not pvc_path_obj.exists():
            raise FileNotFoundError(
                f"PVC source path '{pvc_path}' is not visible to the downloader.\n"
                "The downloader init container only has access to volumes mounted via cacheURI.\n"
                "If the source model and the cache reside on the same PVC, set cacheURI to that\n"
                "PVC name and ensure the modelURI path starts with the cacheURI mount point.\n"
                "Example: cacheURI: pvc://<claimName>, modelURI: pvc:///<claimName>/<path-to-model>"
            )

        if not pvc_path_obj.is_dir():
            raise FileNotFoundError(f"PVC source path '{pvc_path}' exists but is not a directory")

        output_dir_obj.mkdir(parents=True, exist_ok=True)

        cmd = [
            "rsync",
            "-av",
            "--partial",
            "--progress",
            f"{str(pvc_path_obj)}/",
            f"{str(output_dir_obj)}/"
        ]

        logger.info(f"Starting file sync from {pvc_path} to {output_dir}")

        process = subprocess.Popen(
            cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1
        )

        for line in iter(process.stdout.readline, ''):
            if line:
                logger.info(line.strip())

        process.stdout.close()
        return_code = process.wait()

        if return_code != 0:
            errors = process.stderr.read()
            logger.error(f"rsync failed with code {return_code}: {errors}")
            raise subprocess.SubprocessError(f"rsync failed with code {return_code}")

        return True

    def _parse_pvc_path(self) -> str:
        path = self.source_path[6:]
        if not path:
            raise ValueError(f"No path specified in PVC URI: {self.source_path}")

        if not path.startswith('/'):
            path = '/' + path

        return os.path.normpath(path)

    def download(self, output_dir: str):
        try:
            pvc_path = self._parse_pvc_path()
            os.makedirs(output_dir, exist_ok=True)
            self._copy_from_pvc(pvc_path, output_dir)
            logger.info(f"Files copied from {pvc_path} to {output_dir}")
        except FileNotFoundError as e:
            logger.error(f"PVC path error: {str(e)}")
            raise
        except Exception as e:
            logger.error(f"Failed to copy from PVC: {str(e)}")
            raise
