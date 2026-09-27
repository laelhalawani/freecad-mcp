"""CalculiX-driven FEM analysis execution."""

import tempfile
import traceback

import FreeCAD
import ObjectsFem


# FEM solvers are all Fem::FemSolverObjectPython; FreeCAD tells them apart by
# the Python proxy's Type (femtools.femutils.type_of_obj). These are the
# CalculiX solvers ObjectsFem creates, in order of preference:
# makeSolverCalculiXCcxTools, then makeSolverCalculiX. FemToolsCcx is built
# for the ccx tools solver, and the other one carries every property its input
# writer reads. The older solver-framework type "Fem::SolverCalculix" lacks
# some of them (IncrementsMaximum, for one), so an analysis holding only that
# one gets a new ccx tools solver instead.
_CALCULIX_SOLVER_TYPES = ("Fem::SolverCcxTools", "Fem::SolverCalculiX")


def _fem_type(obj) -> str:
    """Return ``obj``'s FEM type the way ``femutils.type_of_obj`` does."""
    proxy = getattr(obj, "Proxy", None)
    proxy_type = getattr(proxy, "Type", None)
    if isinstance(proxy_type, str):
        return proxy_type
    return getattr(obj, "TypeId", "")


def _find_calculix_solver(analysis):
    """Return the preferred CalculiX solver in ``analysis``, or None."""
    members = list(analysis.Group)
    for solver_type in _CALCULIX_SOLVER_TYPES:
        for member in members:
            if _fem_type(member) == solver_type:
                return member
    return None


def run_fem_analysis(doc_name: str, analysis_name: str) -> dict:
    """Run the CalculiX solver on an existing FEM analysis container.

    Always returns a dict with at least ``success`` and ``error``/result keys
    so the caller can pass it through to the wire response unchanged.
    """
    work_dir = None
    stage = "initialization"
    try:
        stage = "document lookup"
        try:
            doc = FreeCAD.getDocument(doc_name)
        except Exception:
            return {"success": False, "error": f"Document '{doc_name}' not found."}
        analysis = doc.getObject(analysis_name)
        if analysis is None:
            return {"success": False, "error": f"Analysis '{analysis_name}' not found."}
        if analysis.TypeId not in ("Fem::FemAnalysis", "Fem::FemAnalysisPython"):
            return {"success": False, "error": f"'{analysis_name}' is not a FEM analysis (TypeId={analysis.TypeId})."}

        stage = "solver resolution"
        solver = _find_calculix_solver(analysis)
        if solver is None:
            solver_factory = (
                getattr(ObjectsFem, "makeSolverCalculiXCcxTools", None)
                or getattr(ObjectsFem, "makeSolverCalculixCcxTools", None)
            )
            if solver_factory is None:
                return {"success": False, "error": "ObjectsFem has no Calculix solver factory."}
            solver = solver_factory(doc, "CalculiX")
            analysis.addObject(solver)

        stage = "femtools import"
        from femtools import ccxtools

        stage = "solver setup"
        fea = ccxtools.FemToolsCcx(analysis=analysis, solver=solver)
        fea.update_objects()

        work_dir = tempfile.mkdtemp(prefix="freecad_mcp_fem_")
        fea.setup_working_dir(work_dir)
        fea.setup_ccx()

        stage = "prerequisite check"
        prereq_msg = fea.check_prerequisites()
        if prereq_msg:
            return {"success": False, "error": f"Prerequisites failed: {prereq_msg}", "working_dir": work_dir}

        stage = "solver execution"
        fea.purge_results()
        # FemToolsCcx.run() reports CalculiX failures by returning False rather
        # than raising; None is returned on success in some FreeCAD versions,
        # so only an explicit False is treated as a failure.
        if fea.run() is False:
            return {
                "success": False,
                "error": "CalculiX solver run failed (fea.run() returned False); inspect the .dat/.frd output in working_dir.",
                "working_dir": work_dir,
            }

        stage = "result loading"
        fea.load_results()

        result_obj = None
        for member in analysis.Group:
            if "Result" in getattr(member, "TypeId", "") and hasattr(member, "vonMises"):
                result_obj = member
                break
        if result_obj is None:
            return {"success": False, "error": "Solver ran but no result object was produced.", "working_dir": work_dir}

        stage = "result extraction"
        # vonMises / DisplacementLengths can be None on a degenerate run.
        vm = list(getattr(result_obj, "vonMises", None) or [])
        disp = list(getattr(result_obj, "DisplacementLengths", None) or [])
        doc.recompute()

        return {
            "success": True,
            "result_object": result_obj.Name,
            "node_count": len(vm),
            "max_von_mises_MPa": max(vm) if vm else None,
            "min_von_mises_MPa": min(vm) if vm else None,
            "max_displacement_mm": max(disp) if disp else None,
            "working_dir": work_dir,
        }
    except Exception as e:
        return {
            "success": False,
            "error": f"FEM analysis failed during {stage}: {type(e).__name__}: {e}",
            "traceback": traceback.format_exc(),
            "working_dir": work_dir,
        }
