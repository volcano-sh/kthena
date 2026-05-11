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

import argparse
import json
import os
from pathlib import Path

from kthena.downloader.downloader import download_model
from kthena.downloader.logger import setup_logger

logger = setup_logger()


def load_config(config_str: str = None) -> dict:
    config = {}
    if config_str:
        try:
            config = json.loads(config_str)
            logger.info("Loaded configuration from JSON string.")
        except json.JSONDecodeError as e:
            logger.error(f"Error parsing configuration JSON: {e}")
            raise ValueError("Invalid configuration JSON format.") from e

    env_config = {
        "hf_token": os.getenv("HF_AUTH_TOKEN"),
        "hf_endpoint": os.getenv("HF_ENDPOINT"),
        "hf_revision": os.getenv("HF_REVISION"),
        "ms_token": os.getenv("MS_TOKEN"),
        "ms_revision": os.getenv("MS_REVISION"),
        "access_key": os.getenv("ACCESS_KEY"),
        "secret_key": os.getenv("SECRET_KEY"),
        "endpoint": os.getenv("ENDPOINT"),
    }
    for key, value in env_config.items():
        if value:
            config.setdefault(key, value)
            logger.info(f"Loaded {key} from environment variables.")
    return config


def parse_arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Universal Model Downloader Tool for AI/ML workflows",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter
    )

    parser.add_argument(
        "-s", "--source",
        type=str,
        required=True,
        help="Model source URI or identifier. Supports multiple sources including: "
             "Hugging Face repositories (format: '<namespace>/<repo_name>' or 'hf://<namespace>/<repo_name>'), "
             "ModelScope (ms://<namespace>/<repo_name>), "
             "S3 buckets (s3://bucket/path), Object Storage (obs://bucket/path) and PVC storage (pvc://path)"
    )
    parser.add_argument(
        "-o", "--output-dir",
        type=str,
        default="~/downloads",
        help="Local directory path where model files will be saved."
    )
    parser.add_argument(
        "-w", "--max-workers",
        type=int,
        default=8,
        help="Maximum number of concurrent workers for downloading files."
    )
    parser.add_argument(
        "-c", "--config",
        type=str,
        default=None,
        help="JSON-formatted configuration string with provider-specific settings. Supported keys include:\n"
             "- hf_token: Authentication token for Hugging Face\n"
             "- hf_endpoint: Custom API endpoint for Hugging Face\n"
             "- hf_revision: Specific model revision/branch to download\n"
             "- ms_token: Authentication token for ModelScope\n"
             "- ms_revision: Specific model revision/branch to download from ModelScope\n"
             "- access_key/secret_key: Cloud provider credentials\n"
             "- endpoint:  obs endpoint URL, for s3, not a required config but a private bucket \n"
             "Example: '{\"hf_token\": \"your_huggingface_token\", \"hf_endpoint\": \"custom_endpoint\", "
             "\"hf_revision\": \"main\", \"ms_token\": \"your_modelscope_token\", \"ms_revision\": \"master\", "
             "\"access_key\": \"your_access_key\", \"secret_key\": \"your_secret_key\", "
             "\"endpoint\": \"your_endpoint_url\"}'"
    )
    args = parser.parse_args()
    args.output_dir = str(Path(args.output_dir).expanduser().resolve())
    logger.info(f"Resolved output directory: {args.output_dir}")
    return args


def main():
    try:
        args = parse_arguments()
        config = load_config(args.config)
        download_model(
            source=args.source,
            output_dir=args.output_dir,
            config=config,
            max_workers=args.max_workers,
        )
    except Exception as e:
        logger.error(f"An error occurred: {e}")
        exit(1)


if __name__ == "__main__":
    main()
