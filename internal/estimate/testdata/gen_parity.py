#!/usr/bin/env python3
"""Write parity.json: avail.py's geometry and assess() on synthetic profiles.

To regenerate (avail.py lives in the tanktemp scratchpad; it is never copied
into this repo):

    python3 -I internal/estimate/testdata/gen_parity.py PATH/TO/avail.py \
        > internal/estimate/testdata/parity.json

The Go tests in internal/estimate read parity.json and assert that vessel,
profile and estimate agree with avail.py. The profiles below are SYNTHETIC
(made up to exercise the model's edges), not readings from the house.
Only avail.py's pure parts are used: the module-level geometry, assess(),
and main() with --geometry; nothing touches the network.
"""
import contextlib
import importlib.util
import io
import json
import platform
import sys

CASES = [
    # name, five temperatures bottom (P1) to top (P5); None is an unknown probe
    ("fully_hot", [60.0, 60.0, 60.0, 60.0, 60.0]),
    ("cold", [10.0, 10.0, 10.0, 10.0, 10.0]),
    ("below_cold", [5.0, 8.0, 9.0, 9.5, 10.0]),
    ("all_equal_45", [45.0, 45.0, 45.0, 45.0, 45.0]),
    ("typical_full", [45.2, 58.9, 60.3, 59.7, 61.4]),
    ("thermocline_low", [15.0, 55.0, 60.0, 61.0, 62.0]),
    ("thermocline_mid", [12.0, 14.0, 58.0, 60.0, 61.0]),
    ("thermocline_high", [11.0, 12.0, 13.0, 50.0, 62.0]),
    ("right_p4_return_dip", [30.0, 55.0, 60.0, 48.0, 61.0]),
    ("cold_p5_at_rest", [40.0, 58.0, 60.0, 60.0, 49.0]),
    ("fully_inverted", [60.0, 50.0, 40.0, 30.0, 20.0]),
    ("p2_nil", [20.0, None, 58.0, 60.0, 61.0]),
    ("p1_nil", [None, 50.0, 58.0, 60.0, 61.0]),
    ("p5_nil", [20.0, 50.0, 58.0, 60.0, None]),
    ("only_two_probes", [None, 30.0, None, None, 60.0]),
    ("one_probe_only", [None, None, 55.0, None, None]),
    ("all_nil", [None, None, None, None, None]),
    ("at_t_min", [40.0, 40.0, 40.0, 40.0, 40.0]),
    ("just_below_t_min", [39.99, 39.99, 39.99, 39.99, 39.99]),
    ("straddling_t_min", [39.999, 40.0, 40.001, 40.0, 41.0]),
    ("crossing_t_min_mid", [20.0, 30.0, 50.0, 55.0, 60.0]),
]


def load(path):
    spec = importlib.util.spec_from_file_location("avail", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def geometry_text(mod):
    buf = io.StringIO()
    argv = sys.argv
    sys.argv = ["avail.py", "--geometry"]
    try:
        with contextlib.redirect_stdout(buf):
            mod.main()
    finally:
        sys.argv = argv
    return buf.getvalue()


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: gen_parity.py PATH/TO/avail.py > parity.json")
    a = load(sys.argv[1])
    sl = a.SLICES
    out = {
        "generator": "internal/estimate/testdata/gen_parity.py",
        "python": platform.python_version(),
        "params": {
            "capacity_l": a.CAP_L,
            "inner_diameter_mm": a.INNER_D,
            "dome_depth_mm": a.DOME_H,
            "z0_mm": a.Z0,
            "slice_mm": 1.0,  # avail.py slices at a fixed 1 mm
            "heights_mm": a.HEIGHTS,
            "t_cold_c": a.T_COLD,
            "t_min_c": a.T_MIN,
            "t_mix_c": a.T_MIX,
            "kwh_per_litre_k": a.KWH_PER_LK,
        },
        "geometry": {
            "sphere_radius_mm": a.R_SPH,
            "dome_l": a.DOME_L,
            "cyl_area_mm2": a.CYL_AREA,
            "straight_mm": a.L_CYL,
            "z_cyl0_mm": a.Z_CYL0,
            "z_cyl1_mm": a.Z_CYL1,
            "z_top_mm": a.Z_TOP,
            "n_slices": len(sl),
            "first_slices": sl[:3],
            "last_slices": sl[-3:],
            # Plain sequential sums (not sum(), which is compensated from
            # Python 3.12), so Go can reproduce them in the same order.
            "sum_l": seqsum(dl for _, dl in sl),
            "sum_z_l": seqsum(z * dl for z, dl in sl),
            "sum_z2_l": seqsum(z * z * dl for z, dl in sl),
            "text": geometry_text(a),
        },
        "cases": [],
    }
    for name, temps in CASES:
        out["cases"].append({
            "name": name,
            "temps_c": temps,
            "raw": a.assess(temps, False),
            "strat": a.assess(temps, True),
        })
    json.dump(out, sys.stdout, indent=1)
    sys.stdout.write("\n")


def seqsum(xs):
    s = 0.0
    for x in xs:
        s += x
    return s


if __name__ == "__main__":
    main()
