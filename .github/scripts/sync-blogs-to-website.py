#!/usr/bin/env python3
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
import os
import re
import shutil
from datetime import datetime

import yaml


def parse_frontmatter(content):
    """Parse YAML frontmatter from markdown file."""
    if not content.startswith("---"):
        return None, content

    parts = content.split("---", 2)
    if len(parts) < 3:
        return None, content

    try:
        frontmatter = yaml.safe_load(parts[1])
        return frontmatter, parts[2]
    except Exception as e:
        print(f"Failed to parse frontmatter: {e}")
        return None, content


def convert_to_hugo_frontmatter(docusaurus_fm, summary=""):
    """Convert Docusaurus YAML frontmatter to Hugo TOML."""
    if not docusaurus_fm:
        return ""

    title = docusaurus_fm.get("title", "")
    date_str = str(docusaurus_fm.get("date", datetime.now().strftime("%Y-%m-%d")))

    try:
        dt = datetime.strptime(date_str, "%Y-%m-%d")
    except ValueError:
        try:
            dt = datetime.strptime(date_str, "%Y-%m-%dT%H:%M:%S%z")
        except ValueError:
            dt = datetime.now()

    datemonth = dt.strftime("%b")
    dateyear = dt.strftime("%Y")
    dateday = dt.day

    authors = docusaurus_fm.get("authors", [])
    if isinstance(authors, str):
        authors = [authors]

    tags = docusaurus_fm.get("tags", [])

    authors_toml = ", ".join([f'"{a}"' for a in authors])
    tags_toml = ", ".join([f'"{t}"' for t in tags])

    title = title.replace('"', '\\"')

    toml = f"""+++
title = "{title}"
description = "{summary}"
subtitle = ""

date = {date_str}
lastmod = {date_str}
datemonth = "{datemonth}"
dateyear = "{dateyear}"
dateday = {dateday}

draft = false
toc = true
type = "posts"
authors = [{authors_toml}]

tags = [{tags_toml}]
summary = "{summary}"

linktitle = "{title}"
[menu.posts]
parent = "tutorials"
weight = 5
+++
"""
    return toml


def process_content(content, image_map, slug):
    """Convert Docusaurus specific JSX to Hugo markdown/shortcodes."""
    content = content.replace("<!-- truncate -->", "<!--more-->")

    safe_slug = slug.replace("/", "-")

    summary = ""
    summary_match = re.search(r"^(.*?)(?:<!--more-->|$)", content.strip(), re.DOTALL)
    if summary_match:
        summary_raw = summary_match.group(1).strip()
        summary = re.sub(r"#+\s*", "", summary_raw)
        summary = re.sub(r"\n+", " ", summary)
        if len(summary) > 200:
            summary = summary[:197] + "..."
        summary = summary.replace('"', '\\"').replace("\n", " ")

    content = re.sub(
        r"import\s+LightboxImage\s+from\s+'@site/src/components/LightboxImage';\s*\n*",
        "",
        content,
    )

    for match in re.finditer(r"import\s+(\w+)\s+from\s+['\"](.*?)['\"];\s*\n*", content):
        var_name = match.group(1)
        rel_path = match.group(2)
        filename = os.path.basename(rel_path)
        hugo_path = f"./kthena-assets/{safe_slug}/{filename}"
        image_map[var_name] = (rel_path, hugo_path)

    content = re.sub(r"import\s+\w+\s+from\s+['\"].*?['\"];\s*\n*", "", content)

    def replace_jsx(match):
        attrs = match.group(1)
        src_match = re.search(r"src=\{([^}]+)\}", attrs)
        alt_match = re.search(r"alt=[\"']([^\"']+)[\"']", attrs)

        if not src_match:
            return match.group(0)

        var_name = src_match.group(1)
        alt_text = alt_match.group(1) if alt_match else ""

        if var_name in image_map:
            hugo_path = image_map[var_name][1]
            return f'{{{{<figure library="1" src="{hugo_path}" alt="{alt_text}">}}}}'
        return match.group(0)

    content = re.sub(r"<LightboxImage\s+(.*?)\s*/>", replace_jsx, content)

    def replace_md_images(match):
        alt = match.group(1)
        path = match.group(2)
        if path.startswith("./") or path.startswith("../"):
            filename = os.path.basename(path)
            hugo_path = f"./kthena-assets/{safe_slug}/{filename}"
            image_map[path] = (path, hugo_path)
            return f'{{{{<figure library="1" src="{hugo_path}" alt="{alt}">}}}}'
        return match.group(0)

    content = re.sub(r"!\[([^\]]*)\]\(([^)]+)\)", replace_md_images, content)

    return content.strip(), summary


def main():
    parser = argparse.ArgumentParser(description="Sync Kthena blogs to Volcano website format")
    parser.add_argument("--source", default="docs/kthena/blog", help="Source blog directory")
    parser.add_argument(
        "--target",
        default="../website/content/en/blog",
        help="Target website blog directory",
    )
    args = parser.parse_args()

    if not os.path.exists(args.source):
        print(f"Source directory {args.source} not found.")
        return

    os.makedirs(args.target, exist_ok=True)

    for root, dirs, files in os.walk(args.source):
        for file in files:
            if file.endswith(".md") or file.endswith(".mdx"):
                if file == "README.md":
                    continue

                filepath = os.path.join(root, file)

                with open(filepath, "r", encoding="utf-8") as f:
                    content = f.read()

                frontmatter, body = parse_frontmatter(content)
                if not frontmatter:
                    continue

                slug = frontmatter.get("slug")
                if not slug:
                    if file == "index.md":
                        slug = os.path.basename(root)
                    else:
                        slug = os.path.splitext(file)[0]

                safe_slug = slug.replace("/", "-")

                target_filename = f"{slug}.md"
                if target_filename.startswith("/"):
                    target_filename = target_filename[1:]

                target_filepath = os.path.join(args.target, target_filename)

                slug_images_dir = os.path.join(args.target, "kthena-assets", safe_slug)
                os.makedirs(slug_images_dir, exist_ok=True)

                image_map = {}
                processed_body, summary = process_content(body, image_map, slug)

                hugo_frontmatter = convert_to_hugo_frontmatter(frontmatter, summary)

                for original_path, (rel_path, hugo_path) in image_map.items():
                    src_image_path = os.path.normpath(os.path.join(root, rel_path))
                    dst_image_path = os.path.join(slug_images_dir, os.path.basename(rel_path))

                    if os.path.exists(src_image_path):
                        shutil.copy2(src_image_path, dst_image_path)
                        print(f"Copied image {src_image_path} -> {dst_image_path}")
                    else:
                        print(f"Warning: Image not found {src_image_path}")

                with open(target_filepath, "w", encoding="utf-8") as f:
                    f.write(hugo_frontmatter)
                    f.write("\n")
                    f.write(processed_body)
                    f.write("\n")

                print(f"Converted {filepath} -> {target_filepath}")


if __name__ == "__main__":
    main()
