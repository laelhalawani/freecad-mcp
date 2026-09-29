"""CalculiX-driven FEM analysis execution."""

import tempfile
import traceback

import FreeCAD
import ObjectsFem

from rpc_server.errors import INVALID_INPUT, NOT_FOUND, fail, tool_call
from rpc_server.fem_loads import analysis_loads
from rpc_server.transactions import active_document, transaction


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


#: The name of the von Mises field of a result pipeline
#: (Mod/Fem/App/FemVTKTools.cpp, "vonMises" to "von Mises Stress").
_VON_MISES_FIELD = "von Mises Stress"


def _hide_meshed_geometry(analysis) -> list[str]:
    """Hide the FEM mesh and the solid it meshes, which sit under the coloured
    result and would hide it. Returns the names hidden."""
    hidden = []
    for member in analysis.Group:
        if _fem_type(member) not in ("Fem::FemMeshGmsh", "Fem::FemMeshNetgen"):
            continue
        targets = [member]
        for attr in ("Shape", "Part"):
            target = getattr(member, attr, None)
            if target is not None and not isinstance(target, (list, tuple)):
                targets.append(target)
        for target in targets:
            view = getattr(target, "ViewObject", None)
            if view is not None and view.Visibility:
                view.Visibility = False
                hidden.append(target.Name)
    return hidden


def _colour_by_von_mises(analysis, result_obj, von_mises) -> dict:
    """Colour the result the way FreeCAD's own result view does, so a
    screenshot taken next shows the stress.

    In FreeCAD 1.x solving loads the result into a post pipeline
    (feminout/importCcxFrdResults.py ``setupPipeline``) whose view object
    colours by its ``Field``; choosing "von Mises Stress" also puts the colour
    bar in the 3D view (Gui/ViewProviderFemPostObject.cpp). Without a
    pipeline the result mesh is coloured node by node instead, as the "Show
    result" panel does (femtaskpanels/task_result_mechanical.py). Returns {}
    when there is no GUI or no way to colour, which never fails the analysis.
    """
    if not von_mises:
        return {}
    try:
        import FreeCADGui  # noqa: F401
    except Exception:
        return {}
    try:
        pipelines = [m for m in analysis.Group if getattr(m, "TypeId", "") == "Fem::FemPostPipeline"]
        coloured = False
        if pipelines:
            view = getattr(pipelines[-1], "ViewObject", None)
            if view is not None and _VON_MISES_FIELD in view.getEnumerationsOfProperty("Field"):
                view.Field = _VON_MISES_FIELD
                view.Visibility = True
                coloured = True
        if not coloured:
            mesh_obj = getattr(result_obj, "Mesh", None)
            view = getattr(mesh_obj, "ViewObject", None)
            node_numbers = list(getattr(result_obj, "NodeNumbers", None) or [])
            if view is None or mesh_obj.FemMesh.NodeCount != len(node_numbers):
                return {}
            view.show()
            view.setNodeColorByScalars(node_numbers, list(von_mises))
        hidden = _hide_meshed_geometry(analysis)
    except Exception as e:
        FreeCAD.Console.PrintWarning(f"MCP RPC: could not colour the FEM result: {type(e).__name__}: {e}\n")
        return {}
    return {
        "coloured": True,
        "colour_field": _VON_MISES_FIELD,
        "colour_range_MPa": [min(von_mises), max(von_mises)],
        "hidden_objects": hidden,
    }


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
            return fail(
                NOT_FOUND,
                f"Document '{doc_name}' not found.",
                "Call " + tool_call("list_documents", {}) + " to see the open documents.",
            )
        analysis = doc.getObject(analysis_name)
        if analysis is None:
            return fail(
                NOT_FOUND,
                f"Analysis '{analysis_name}' not found.",
                "Call " + tool_call("list_objects", {"doc_name": doc_name}) + " to see the document's objects.",
            )
        if analysis.TypeId not in ("Fem::FemAnalysis", "Fem::FemAnalysisPython"):
            return fail(
                INVALID_INPUT,
                f"'{analysis_name}' is not a FEM analysis (TypeId={analysis.TypeId}).",
                "Call " + tool_call("list_objects", {"doc_name": doc_name}) + " to see the document's objects.",
            )

        # Everything from here on can add or change document objects (the
        # solver, its results): keep it inside one undoable transaction, which
        # commits on every exit, including the early "failed" returns below,
        # so a partially-run analysis stays undoable in one step. active_document
        # holds doc active for as long as that transaction can be open, else
        # FreeCAD can open an empty linked "-> run_fem_analysis" transaction in
        # whatever document the GUI has focused (App/Document.cpp:379-386).
        with active_document(doc), transaction("run_fem_analysis") as tx:
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

            reply = {
                "success": True,
                "result_object": result_obj.Name,
                "node_count": len(vm),
                "max_von_mises_MPa": max(vm) if vm else None,
                "min_von_mises_MPa": min(vm) if vm else None,
                "max_displacement_mm": max(disp) if disp else None,
                "loads": analysis_loads(analysis),
                "working_dir": work_dir,
                **tx.reply_fields(),
            }
            colouring = _colour_by_von_mises(analysis, result_obj, vm)
            if colouring:
                reply.update(colouring)
            return reply
    except Exception as e:
        return {
            "success": False,
            "error": f"FEM analysis failed during {stage}: {type(e).__name__}: {e}",
            "traceback": traceback.format_exc(),
            "working_dir": work_dir,
        }
