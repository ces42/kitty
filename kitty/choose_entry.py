#!/usr/bin/env python
# License: GPLv3 Copyright: 2019, Kovid Goyal <kovid at kovidgoyal.net>

import re
from collections.abc import Generator
from typing import Any

from .cli_stub import HintsCLIOptions
from .typing import MarkType


def mark(text: str, args: HintsCLIOptions, Mark: type[MarkType], extra_cli_args: list[str], *a: Any) -> Generator[MarkType, None, None]:
    idx = 0
    found_start_line = False
    for m in re.finditer(r'(?m)^.+$', text):
        start, end = m.span()
        line = text[start:end].replace('\0', '').replace('\n', '')
        if line == ' ':
            found_start_line = True
            continue
        # if line.startswith(': '):
        if line.lstrip().startswith(': '):
            yield Mark(idx, start + line.find(':'), end, line, {'index': idx})
            idx += 1
        elif found_start_line:
            # skip this line incrementing the index
            idx += 1
